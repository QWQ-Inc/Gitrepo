package server_test

import (
	"bytes"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/client"
)

// Relocate forgets release_mirrors under the repository's sync lease, but
// release publishing does not take that lease. A publication in flight to
// the old location saves its "synced" record after ForgetRemoteState ran;
// once the target points at the new location, that record (old release ID,
// synced hash) is taken as the state of the new one, so the release is
// never published there.
func TestReleaseStateDroppedWhenMirrorMoves(t *testing.T) {
	e := newEnv(t)
	fake := newFakePlatforms(t)
	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "github", "provider": "github", "namespace": "acme", "base_url": fake.srv.URL + "/gh",
		"credential": map[string]any{"kind": "token", "secret": ghToken},
	}, nil)
	e.do(http.MethodPost, "/api/bindings", map[string]any{"org": "acme", "target": "github"}, nil)
	var dev client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	e.commit(work, "main.go", "package main\n")
	e.run(work, "tag", "-a", "-m", "v1.0", "v1.0")
	mustContain(t, e.run(work, "push", e.hubURL(dev.Token, "acme/svc"), "main", "v1.0"), "[ok]       github:acme/svc")

	const rel = "/api/repos/acme/svc/releases"
	e.do(http.MethodPost, rel, map[string]any{"tag": "v1.0", "name": "One"}, nil)
	req, _ := http.NewRequest(http.MethodPut, "http://"+e.hubAddr+rel+"/v1.0/assets/app.bin", bytes.NewReader([]byte("binary")))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 201 {
		t.Fatalf("upload asset: %v %v", err, resp)
	}

	fake.inject("slow", "gh", true)
	published := make(chan relOut, 1)
	go func() {
		var r relOut
		e.do(http.MethodPost, rel+"/v1.0/publish", map[string]any{"wait": true}, &r)
		published <- r
	}()
	time.Sleep(400 * time.Millisecond) // uploading app.bin to github:acme/svc
	e.do(http.MethodPatch, "/api/targets/github", map[string]any{"namespace": "acme2"}, nil)
	r := <-published
	fake.inject("slow", "gh", false)
	t.Logf("publication that was in flight: %+v", r.Mirrors)

	ev := eventState(e.sync("acme/svc", false), "github")
	t.Logf("sync after the move: %+v", ev)
	if !fake.tagExists("gh", "acme2/svc", "v1.0") {
		t.Fatalf("the tag did not reach the new location")
	}
	e.do(http.MethodPost, rel+"/v1.0/publish", map[string]any{"wait": true}, &r)
	t.Logf("publish again after the move: %+v", r.Mirrors)
	if fake.release("gh", "acme2/svc", "v1.0") == nil {
		t.Errorf("BUG: the release was never published at the new location github:acme2/svc (state carried over from acme/svc)")
	}
}
