package server_test

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/client"
)

// A rolling update (Kubernetes, PaaS platforms) starts the new instance on
// the same data directory before it stops the old one. The old instance's
// shutdown must not take the new one's internal socket with it: every git
// subprocess (hooks, askpass for mirror credentials) talks to the server
// through it, so pushes and syncs failed with "dial unix …/gitrepo.sock:
// no such file or directory" until the next restart.
func TestRollingUpdateKeepsInternalSocket(t *testing.T) {
	e := newEnv(t)
	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "mirror-a", "provider": "generic", "url_template": e.remoteURL + "/a/{name}.git",
		"credential": map[string]any{"kind": "basic", "username": userA, "secret": passA},
	}, nil)
	e.do(http.MethodPost, "/api/bindings", map[string]any{"org": "acme", "target": "mirror-a"}, nil)
	var dev client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)

	// Sockets of instances that were killed (no one listens on them) are
	// removed when an instance starts; the old instance's live one stays.
	run := e.cfg.RunDir()
	live, _ := filepath.Glob(filepath.Join(run, "gitrepo-*.sock"))
	if len(live) != 1 {
		t.Fatalf("sockets: %v", live)
	}
	var stale []string
	for _, name := range []string{"gitrepo.sock", "gitrepo-deadbeef.sock"} {
		p := filepath.Join(run, name)
		ln, err := net.Listen("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		ln.(*net.UnixListener).SetUnlinkOnClose(false)
		ln.Close()
		stale = append(stale, p)
	}

	// The new instance starts on the same data directory…
	cfg := *e.cfg
	_, addr := startServer(t, &cfg, slog.New(slog.NewTextHandler(e.logs, nil)))
	for _, p := range stale {
		if _, err := os.Lstat(p); err == nil {
			t.Fatalf("stale socket %s was kept", p)
		}
	}
	if _, err := os.Lstat(live[0]); err != nil {
		t.Fatalf("the running instance's socket was removed: %v", err)
	}
	// …and then the old one stops.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	// A push to the new instance runs its hooks, and the sync to the
	// mirror its askpass helper: both through the internal socket.
	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	sha := e.commit(work, "README.md", "after the handover\n")
	e.run(work, "push", "-q", "http://x:"+dev.Token+"@"+addr+"/acme/demo.git", "main")
	if got := e.refs(e.remoteRepo("a/demo.git"))["refs/heads/main"]; got != sha {
		t.Fatalf("the mirror has %q, want %s: the new instance cannot reach its own helpers", got, sha)
	}
}
