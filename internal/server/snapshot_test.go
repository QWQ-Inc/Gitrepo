package server_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwq-inc/gitrepo/internal/client"
	"github.com/qwq-inc/gitrepo/internal/gitexec"
	"github.com/qwq-inc/gitrepo/internal/lfs"
	"github.com/qwq-inc/gitrepo/internal/releases"
	"github.com/qwq-inc/gitrepo/internal/snapshot"
)

func TestSnapshotRoundTrip(t *testing.T) {
	e := newEnv(t)
	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "mirror-a", "provider": "generic", "url_template": e.remoteURL + "/a/{name}.git",
		"credential": map[string]any{"kind": "basic", "username": userA, "secret": passA},
	}, nil)
	e.do(http.MethodPost, "/api/bindings", map[string]any{"org": "acme", "target": "mirror-a"}, nil)
	if err := e.git.InitBare(t.Context(), e.remoteRepo("a/app.git"), "main"); err != nil {
		t.Fatal(err)
	}
	var dev client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)

	// History with a branch, a tag, an overwritten commit (hidden backup)
	// and an LFS object; plus an empty repository.
	hub := e.hubURL(dev.Token, "acme/app")
	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	e.commit(work, "a.txt", "1\n")
	e.run(work, "push", "-q", hub, "main")
	e.commit(work, "a.txt", "oops\n")
	e.run(work, "push", "-q", hub, "main")
	e.run(work, "reset", "-q", "--hard", "HEAD~1")
	e.run(work, "tag", "v1")
	e.run(work, "push", "-q", "--force", hub, "main", "v1")
	e.run(work, "push", "-q", hub, "main:dev")
	hubDir := e.hubRepo("acme/app")
	e.run(hubDir, "symbolic-ref", "HEAD", "refs/heads/dev") // a non-default HEAD must survive
	blob := []byte("large binary\n")
	oid := lfsOID(blob)
	putLFS(t, hubDir, oid, blob)
	e.do(http.MethodPost, "/api/repos", map[string]any{"slug": "acme/empty"}, nil)
	e.do(http.MethodPost, "/api/repos/acme/app/releases", map[string]any{"tag": "v1"}, nil)
	relFile := []byte("release binary\n")
	req, _ := http.NewRequest(http.MethodPut, "http://"+e.hubAddr+"/api/repos/acme/app/releases/v1/assets/app.bin", bytes.NewReader(relFile))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 201 {
		t.Fatalf("upload release file: %v %v", err, resp)
	}

	g := &gitexec.Git{Bin: "git", Env: clientEnv()}
	snap := filepath.Join(t.TempDir(), "snap")
	var progress bytes.Buffer
	m, err := snapshot.Create(t.Context(), e.cfg, g, snap, "test", &progress)
	if err != nil {
		t.Fatalf("snapshot of a running server: %v\n%s", err, progress.String())
	}
	orig := e.allRefs(hubDir)
	if !hasPrefix(orig, "refs/backup/") || len(m.Repos) != 2 {
		t.Fatalf("setup: refs %v, manifest %+v", orig, m)
	}
	for _, r := range m.Repos {
		switch r.Slug {
		case "acme/app":
			if r.Refs != len(orig) || r.LFSObjects != 1 || r.ReleaseFiles != 1 || r.Head != "refs/heads/dev" || r.Bundle == "" {
				t.Fatalf("manifest entry: %+v", r)
			}
		case "acme/empty":
			if r.Bundle != "" || r.Refs != 0 {
				t.Fatalf("manifest entry: %+v", r)
			}
		}
	}
	if fi, err := os.Stat(snap); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("snapshot directory must be private: %v %v", fi.Mode(), err)
	}
	if _, err := snapshot.Create(t.Context(), e.cfg, g, snap, "test", nil); err == nil {
		t.Fatal("a snapshot overwrote an existing one")
	}

	cfg2 := *e.cfg
	cfg2.DataDir = filepath.Join(t.TempDir(), "restored")
	if _, err := snapshot.Restore(t.Context(), &cfg2, g, snap, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.Restore(t.Context(), &cfg2, g, snap, nil); err == nil || !strings.Contains(err.Error(), "new data directory") {
		t.Fatalf("restore over an existing instance: %v", err)
	}

	restoredApp := filepath.Join(cfg2.ReposDir(), "acme", "app.git")
	t.Run("repositories come back with every ref, HEAD and LFS object", func(t *testing.T) {
		sameRefs(t, "restored refs", e.allRefs(restoredApp), orig)
		if head := strings.TrimSpace(e.run(restoredApp, "symbolic-ref", "HEAD")); head != "refs/heads/dev" {
			t.Fatalf("HEAD = %s", head)
		}
		if got, err := os.ReadFile(lfs.ObjectPath(restoredApp, oid)); err != nil || !bytes.Equal(got, blob) {
			t.Fatalf("LFS object: %v %q", err, got)
		}
		if _, err := os.Stat(filepath.Join(cfg2.ReposDir(), "acme", "empty.git", "HEAD")); err != nil {
			t.Fatal("empty repository not restored")
		}
		if got, err := os.ReadFile(releases.BlobPath(restoredApp, lfsOID(relFile))); err != nil || !bytes.Equal(got, relFile) {
			t.Fatalf("release file: %v %q", err, got)
		}
	})

	log := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	_, addr := startServer(t, &cfg2, log)
	restored := client.New(cfg2.PublicURL, adminToken)

	t.Run("the restored server keeps tokens, credentials, mirrors and the audit log", func(t *testing.T) {
		url := "http://x:" + dev.Token + "@" + addr + "/acme/app.git"
		clone := filepath.Join(t.TempDir(), "clone")
		e.run("", "clone", "-q", "-b", "main", url, clone)
		sha := e.commit(clone, "b.txt", "after restore\n")
		out := e.run(clone, "push", url, "main")
		mustContain(t, out, "[ok]")
		if got := e.refs(e.remoteRepo("a/app.git"))["refs/heads/main"]; got != sha {
			t.Fatalf("mirror not updated through restored credentials: %s != %s\n%s", got, sha, out)
		}
		var page auditPage
		if err := restored.Do(t.Context(), http.MethodGet, "/api/audit?action=push", nil, &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Entries) < 5 {
			t.Fatalf("audit log not restored: %+v", page.Entries)
		}
	})
}

func hasPrefix(refs map[string]string, prefix string) bool {
	for r := range refs {
		if strings.HasPrefix(r, prefix) {
			return true
		}
	}
	return false
}
