package server_test

import (
	"context"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/client"
)

// relocEnv: target mirror-a (generic, a/{name}.git) bound to acme, and two
// repositories acme/one (main + feature) and acme/two (main) synced to it.
// c/one.git and c/two.git exist; c/one.git holds someone else's project.
func relocEnv(t *testing.T) (e *env, dev client.Token, work, theirs string) {
	e = newEnv(t)
	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "mirror-a", "provider": "generic", "url_template": e.remoteURL + "/a/{name}.git",
		"credential": map[string]any{"kind": "basic", "username": userA, "secret": passA},
	}, nil)
	e.do(http.MethodPost, "/api/bindings", map[string]any{"org": "acme", "target": "mirror-a"}, nil)
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	for _, p := range []string{"a/one.git", "a/two.git", "c/one.git", "c/two.git"} {
		if err := e.git.InitBare(t.Context(), e.remoteRepo(p), "main"); err != nil {
			t.Fatal(err)
		}
	}
	work = filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	e.commit(work, "a.txt", "1\n")
	e.run(work, "branch", "feature")
	mustContain(t, e.run(work, "push", e.hubURL(dev.Token, "acme/one"), "main", "feature"), "[ok]       mirror-a:one")
	mustContain(t, e.run(work, "push", e.hubURL(dev.Token, "acme/two"), "main"), "[ok]       mirror-a:two")

	foreign := filepath.Join(t.TempDir(), "foreign")
	e.run("", "init", "-q", "-b", "main", foreign)
	theirs = e.commit(foreign, "theirs.txt", "not yours\n")
	e.run(foreign, "push", "-q", e.remoteRepo("c/one.git"), "main")
	return e, dev, work, theirs
}

func holdRepoLease(t *testing.T, e *env, slug string) (release func()) {
	t.Helper()
	ctx := context.Background()
	repo, err := e.srv.Store().GetRepoBySlug(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}
	name := "repo:" + strconv.FormatInt(repo.ID, 10)
	ok, err := e.srv.Store().AcquireLease(ctx, name, "review8-long-sync", (10 * time.Minute).Milliseconds())
	if err != nil || !ok {
		t.Fatalf("acquire lease: %v %v", ok, err)
	}
	return func() { e.srv.Store().ReleaseLease(ctx, name, "review8-long-sync") }
}

// A target change that moves mirrors waits until no sync of any affected
// repository runs, and then switches the location and forgets the old
// one's state in one step: nothing is changed while it waits, and no sync
// can learn about the old location in between and carry that over to the
// new one (which would skip the adoption check there).
func TestTargetMoveIsAtomic(t *testing.T) {
	e, _, _, theirs := relocEnv(t)
	remOne := e.remoteByTarget("acme/one", "mirror-a")
	releaseTwo := holdRepoLease(t, e, "acme/two") // a long sync of acme/two

	patched := make(chan error, 1)
	go func() {
		patched <- e.api.Do(context.Background(), http.MethodPatch, "/api/targets/mirror-a",
			map[string]any{"url_template": e.remoteURL + "/c/{name}.git"}, nil)
	}()
	time.Sleep(700 * time.Millisecond)
	// While it waits: nothing has changed yet.
	var tgt client.Target
	e.do(http.MethodGet, "/api/targets/mirror-a", nil, &tgt)
	if !strings.Contains(tgt.URLTemplate, "/a/{name}.git") {
		t.Fatalf("the target changed before the move could be done safely: %s", tgt.URLTemplate)
	}
	if ps, _ := e.srv.Store().GetPushedState(context.Background(), remOne.ID); len(ps) == 0 {
		t.Fatal("acme/one's state was forgotten before the move")
	}
	releaseTwo()
	if err := <-patched; err != nil {
		t.Fatalf("PATCH target: %v", err)
	}
	ev := eventState(e.sync("acme/one", false), "mirror-a")
	newRefs := e.refs(e.remoteRepo("c/one.git"))
	if !strings.Contains(ev.Error, "adoption required") {
		t.Errorf("the new location holding someone else's project was not put through the adoption check (state %q, error %q)", ev.State, ev.Error)
	}
	if _, ok := newRefs["refs/heads/feature"]; ok || newRefs["refs/heads/main"] != theirs {
		t.Errorf("Gitrepo wrote into someone else's repository at the new location: %v", newRefs)
	}
}

// A target change that cannot be done (a sync does not finish, or the
// request is given up) changes nothing: not the target, and not what
// Gitrepo knows about mirrors that stay where they are.
func TestRefusedTargetMoveChangesNothing(t *testing.T) {
	e, dev, work, _ := relocEnv(t)
	remOne := e.remoteByTarget("acme/one", "mirror-a")
	releaseTwo := holdRepoLease(t, e, "acme/two")
	defer releaseTwo()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	err := e.api.Do(ctx, http.MethodPatch, "/api/targets/mirror-a", map[string]any{"url_template": e.remoteURL + "/c/{name}.git"}, nil)
	cancel()
	t.Logf("PATCH target (client gave up after 3s): %v", err)

	var page auditPage
	deadline := time.Now().Add(20 * time.Second)
	for {
		e.do(http.MethodGet, "/api/audit?action=targets", nil, &page)
		if row := page.find(func(r auditRow) bool { return r.Status >= 400 }); row != nil {
			t.Logf("audit: status %d %q", row.Status, row.Detail)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the target update never finished")
		}
		time.Sleep(50 * time.Millisecond)
	}
	var tgt client.Target
	e.do(http.MethodGet, "/api/targets/mirror-a", nil, &tgt)
	if !strings.Contains(tgt.URLTemplate, "/a/{name}.git") {
		t.Fatalf("the target was changed: %s", tgt.URLTemplate)
	}
	ps, _ := e.srv.Store().GetPushedState(context.Background(), remOne.ID)
	rem, _ := e.srv.Store().GetRemote(context.Background(), remOne.ID)
	t.Logf("acme/one's mirror after the refused target change: pushed state %v, adopt %v, provisioned %v", ps, rem.Adopt, rem.Provisioned)
	if len(ps) == 0 {
		t.Errorf("the target was not changed, but what Gitrepo knew about acme/one's (unchanged) mirror was forgotten")
	}

	// The next push to acme/one: its mirror is the same repository Gitrepo
	// has always written to.
	e.commit(work, "a.txt", "2\n")
	out := e.run(work, "push", e.hubURL(dev.Token, "acme/one"), "main")
	t.Logf("next push to acme/one:\n%s", out)
	if strings.Contains(out, "adoption required") {
		t.Errorf("acme/one's own, unmoved mirror now fails with \"adoption required\"")
	}
}
