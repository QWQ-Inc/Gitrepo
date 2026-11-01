package server_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/client"
	"github.com/qwq-inc/gitrepo/internal/lfs"
)

type backupEntry struct {
	Ref      string `json:"ref"`
	Source   string `json:"source"`
	Original string `json:"original"`
	SHA      string `json:"sha"`
	Subject  string `json:"subject"`
}

// ageObjects backdates every object file so the purge's five-minute grace
// period does not protect the test's freshly pushed content.
func ageObjects(t *testing.T, dir string) {
	old := time.Now().Add(-time.Hour)
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			os.Chtimes(p, old, old)
		}
		return nil
	})
}

func TestBackupManagement(t *testing.T) {
	e := newEnv(t)
	var dev client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	hub := e.hubURL(dev.Token, "acme/leak")
	hubDir := e.hubRepo("acme/leak")
	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	e.commit(work, "app.txt", "app\n")
	e.run(work, "push", "-q", hub, "main")
	e.commit(work, "secret.txt", "AWS_SECRET=hunter2\n")
	secret := strings.TrimSpace(e.run(work, "rev-parse", "HEAD"))
	e.run(work, "push", "-q", hub, "main")
	e.run(work, "reset", "-q", "--hard", "HEAD~1")
	e.run(work, "push", "-q", "--force", hub, "main")

	var list []backupEntry
	t.Run("backups are listed", func(t *testing.T) {
		e.do(http.MethodGet, "/api/repos/acme/leak/backups", nil, &list)
		if len(list) != 1 || list[0].Source != "hub" || list[0].Original != "refs/heads/main" || list[0].SHA != secret || list[0].Subject != "change secret.txt" {
			t.Fatalf("backups: %+v", list)
		}
	})

	t.Run("a backup can be restored as a new branch", func(t *testing.T) {
		var res struct{ Branch, SHA string }
		e.do(http.MethodPost, "/api/repos/acme/leak/backups/restore", map[string]any{"backup": list[0].Ref}, &res)
		if !strings.HasPrefix(res.Branch, "restore/main-") || res.SHA != secret || e.refs(hubDir)["refs/heads/"+res.Branch] != secret {
			t.Fatalf("restore: %+v", res)
		}
		if err := e.api.Do(t.Context(), http.MethodPost, "/api/repos/acme/leak/backups/restore", map[string]any{"backup": list[0].Ref, "branch": res.Branch}, nil); err == nil {
			t.Fatal("restoring over an existing branch succeeded")
		}
		e.run(work, "push", "-q", hub, ":"+res.Branch) // undo: this creates another backup
	})

	t.Run("before a purge the commit is still fetchable by SHA (protocol v2)", func(t *testing.T) {
		probe := filepath.Join(t.TempDir(), "probe")
		e.run("", "init", "-q", probe)
		if out, err := gitCmd(probe, "-c", "protocol.version=2", "fetch", "-q", hub, secret); err != nil {
			t.Fatalf("expected the documented exposure: %v %s", err, out)
		}
	})

	t.Run("purge removes the content from the hub", func(t *testing.T) {
		if err := e.api.Do(t.Context(), http.MethodPost, "/api/repos/acme/leak/backups/purge", map[string]any{}, nil); err == nil {
			t.Fatal("a purge without a selection or all=true was accepted")
		}
		ageObjects(t, filepath.Join(hubDir, "objects"))
		var res struct{ Backups int }
		e.do(http.MethodPost, "/api/repos/acme/leak/backups/purge", map[string]any{"all": true}, &res)
		if res.Backups != 2 {
			t.Fatalf("purged %d backups", res.Backups)
		}
		if e.git.HasObject(t.Context(), hubDir, secret) {
			t.Fatal("the purged commit is still in the hub")
		}
		probe := filepath.Join(t.TempDir(), "probe")
		e.run("", "init", "-q", probe)
		if out, err := gitCmd(probe, "-c", "protocol.version=2", "fetch", "-q", hub, secret); err == nil {
			t.Fatalf("the purged commit is still fetchable by SHA:\n%s", out)
		}
		clone := filepath.Join(t.TempDir(), "clone")
		e.run("", "clone", "-q", hub, clone)
		if _, err := os.Stat(filepath.Join(clone, "app.txt")); err != nil {
			t.Fatal("purge damaged the published history")
		}
	})

	t.Run("purge with lfs removes only unreferenced LFS objects", func(t *testing.T) {
		keepContent := []byte("kept large file")
		keepOID := lfsOID(keepContent)
		putLFS(t, hubDir, keepOID, keepContent)
		os.WriteFile(filepath.Join(work, "kept.bin"), []byte("version https://git-lfs.github.com/spec/v1\noid sha256:"+keepOID+"\nsize "+itoa(int64(len(keepContent)))+"\n"), 0o644)
		e.run(work, "add", "kept.bin")
		e.run(work, "commit", "-q", "-m", "kept lfs file")
		e.run(work, "push", "-q", hub, "main")
		orphan := []byte("leaked large file")
		orphanOID := lfsOID(orphan)
		putLFS(t, hubDir, orphanOID, orphan)
		ageObjects(t, filepath.Join(hubDir, "lfs"))
		var res struct {
			LFSObjects int `json:"lfs_objects"`
		}
		e.do(http.MethodPost, "/api/repos/acme/leak/backups/purge", map[string]any{"all": true, "lfs": true}, &res)
		if res.LFSObjects != 1 || lfs.Has(hubDir, orphanOID, -1) || !lfs.Has(hubDir, keepOID, -1) {
			t.Fatalf("lfs purge: removed %d, orphan present %v, kept present %v", res.LFSObjects, lfs.Has(hubDir, orphanOID, -1), lfs.Has(hubDir, keepOID, -1))
		}
	})
}

func lfsOID(b []byte) string { return sha256hex(b) }

func putLFS(t *testing.T, repo, oid string, content []byte) {
	p := lfs.ObjectPath(repo, oid)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
