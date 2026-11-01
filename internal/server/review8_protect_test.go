package server_test

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/client"
)

// A rule that only refuses force-pushes ("--allow-delete", or a tag rule
// with deny_delete=false) must not be defeated in two pushes: deleting the
// ref is allowed, but re-creating it without its earlier history (or a tag
// on another commit) is refused like a force-push.
func TestForceRuleSurvivesDeleteAndRecreate(t *testing.T) {
	e := newEnv(t)
	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "mirror-a", "provider": "generic", "url_template": e.remoteURL + "/a/{name}.git",
		"credential": map[string]any{"kind": "basic", "username": userA, "secret": passA},
	}, nil)
	e.do(http.MethodPost, "/api/bindings", map[string]any{"org": "acme", "target": "mirror-a"}, nil)
	var dev client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	hub := e.hubURL(dev.Token, "acme/demo")
	mirror := e.remoteRepo("a/demo.git")
	e.do(http.MethodPost, "/api/protections", map[string]any{"repos": "*", "ref": "develop", "deny_force": true, "deny_delete": false}, nil)
	e.do(http.MethodPost, "/api/protections", map[string]any{"repos": "*", "ref": "refs/tags/v*", "deny_force": true, "deny_delete": false}, nil)

	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	e.commit(work, "m.txt", "m\n")
	e.run(work, "push", "-q", hub, "main") // HEAD of the hub repository
	e.run(work, "checkout", "-q", "-b", "develop")
	first := e.commit(work, "a.txt", "1\n")
	second := e.commit(work, "a.txt", "2\n")
	e.run(work, "tag", "v1.0")
	mustContain(t, e.run(work, "push", hub, "develop", "v1.0"), "[ok]       mirror-a:demo")

	e.run(work, "reset", "-q", "--hard", first)
	rewritten := e.commit(work, "b.txt", "rewritten\n")
	if out, err := gitCmd(work, "push", "-f", hub, "develop"); err == nil || !strings.Contains(out, "force-pushes") {
		t.Fatalf("control: force-push was not refused: %v\n%s", err, out)
	}
	// Deleting is allowed by these rules...
	e.run(work, "push", "-q", hub, ":develop")
	e.run(work, "push", "-q", hub, ":refs/tags/v1.0")
	// ...re-creating with other history is not.
	if out, err := gitCmd(work, "push", hub, "develop"); err == nil || !strings.Contains(out, "without its earlier history") {
		t.Fatalf("re-creating develop with rewritten history: %v\n%s", err, out)
	}
	e.run(work, "tag", "-f", "v1.0", rewritten)
	if out, err := gitCmd(work, "push", hub, "v1.0"); err == nil || !strings.Contains(out, "cannot be re-created on another commit") {
		t.Fatalf("re-creating v1.0 on another commit: %v\n%s", err, out)
	}
	if got := e.refs(mirror); got["refs/heads/develop"] == rewritten || got["refs/tags/v1.0"] == rewritten {
		t.Fatalf("a rewrite reached the mirror: %v", got)
	}
	// Re-creating them as they were (or moving on from there) is fine.
	e.run(work, "checkout", "-q", "-B", "develop", second)
	third := e.commit(work, "a.txt", "3\n")
	e.run(work, "tag", "-f", "v1.0", second)
	e.run(work, "push", "-q", hub, "develop", "v1.0")
	eventually(t, "mirror synced", 10*time.Second, func() bool {
		got := e.refs(mirror)
		return got["refs/heads/develop"] == third && got["refs/tags/v1.0"] != ""
	})
}

// A repository pattern without "/" (e.g. the org name "acme") can never
// match a repository; it is refused rather than accepted as a rule that
// silently protects nothing.
func TestProtectionRepoPatternNeedsSlash(t *testing.T) {
	e := newEnv(t)
	for _, repos := range []string{"acme", "acme/", "/x", "a/b/c"} {
		err := e.api.Do(t.Context(), http.MethodPost, "/api/protections", map[string]any{"repos": repos, "ref": "main"}, nil)
		var apiErr *client.Error
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
			t.Errorf("repos %q: %v (want 400)", repos, err)
		}
	}
	for _, repos := range []string{"*", "*/*", "acme/*", "acme/app"} {
		var p struct {
			ID int64 `json:"id"`
		}
		e.do(http.MethodPost, "/api/protections", map[string]any{"repos": repos, "ref": "main"}, &p)
	}
	// Tokens are checked the same way.
	if err := e.api.Do(t.Context(), http.MethodPost, "/api/tokens", map[string]any{"name": "x", "scopes": []string{"read"}, "repos": []string{"acme"}}, nil); err == nil {
		t.Error("a token limited to the pattern \"acme\" (which matches nothing) was created")
	}
}
