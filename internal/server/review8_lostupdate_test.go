package server_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/client"
)

// A target change that waits (for the leases of repositories whose
// mirrors move, for the platform's expiry API) must not overwrite a change
// another request stored meanwhile: it is refused, and can be retried.
func TestTargetUpdateDoesNotRevertConcurrentChange(t *testing.T) {
	e, _, _, _ := relocEnv(t)
	releaseTwo := holdRepoLease(t, e, "acme/two") // a long sync of acme/two

	slow := make(chan error, 1)
	go func() {
		slow <- e.api.Do(context.Background(), http.MethodPatch, "/api/targets/mirror-a",
			map[string]any{"url_template": e.remoteURL + "/c/{name}.git"}, nil)
	}()
	time.Sleep(500 * time.Millisecond) // the first PATCH is now waiting for acme/two

	// Meanwhile another administrator rotates the credential and sets its
	// expiry by hand (moving no mirror, so it does not wait).
	e.do(http.MethodPatch, "/api/targets/mirror-a", map[string]any{
		"credential": map[string]any{"kind": "basic", "username": "rotated-user", "secret": "rotated-secret"},
		"expires_at": "2027-03-01",
	}, nil)

	releaseTwo()
	err := <-slow
	var apiErr *client.Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict {
		t.Fatalf("the first PATCH, overtaken by another change: %v (want 409)", err)
	}
	var end client.Target
	e.do(http.MethodGet, "/api/targets/mirror-a", nil, &end)
	if end.CredUsername != "rotated-user" || end.ExpiresAt == nil || !strings.Contains(end.URLTemplate, "/a/{name}.git") {
		t.Fatalf("after the refused PATCH: username %q, expires %v, url_template %q", end.CredUsername, end.ExpiresAt, end.URLTemplate)
	}
	// Retried, it applies on top of the rotation.
	e.do(http.MethodPatch, "/api/targets/mirror-a", map[string]any{"url_template": e.remoteURL + "/c/{name}.git"}, &end)
	if end.CredUsername != "rotated-user" || end.ExpiresAt == nil || !strings.Contains(end.URLTemplate, "/c/{name}.git") {
		t.Fatalf("after the retry: username %q, expires %v, url_template %q", end.CredUsername, end.ExpiresAt, end.URLTemplate)
	}
}
