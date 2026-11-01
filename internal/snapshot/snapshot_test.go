package snapshot

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwq-inc/gitrepo/internal/gitexec"
)

func TestReadManifestRejectsUnsafeEntries(t *testing.T) {
	for _, body := range []string{
		`{"format":1,"repos":[{"slug":"../../etc/x"}]}`,
		`{"format":1,"repos":[{"slug":"acme/app","bundle":"../../secret.bundle"}]}`,
		`{"format":1,"repos":[{"slug":"acme/a/b"}]}`,
		`{"format":2,"repos":[]}`,
	} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(body), 0o600)
		if _, err := ReadManifest(dir); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
	if _, err := ReadManifest(t.TempDir()); err == nil || !strings.Contains(err.Error(), "not a complete snapshot") {
		t.Errorf("missing manifest: %v", err)
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A ref written between listing the refs and creating the bundle (a push,
// a backup ref) must not make the snapshot unrestorable.
func TestBundleCountsItsOwnRefs(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "r.git")
	gitIn(t, dir, "init", "-q", "--bare", "-b", "main", repo)
	tree := gitIn(t, repo, "mktree")
	commit := gitIn(t, repo, "commit-tree", tree, "-m", "c")
	gitIn(t, repo, "update-ref", "refs/heads/main", commit)

	wrapper := filepath.Join(dir, "git-wrapper")
	os.WriteFile(wrapper, []byte(`#!/bin/sh
case " $* " in *" bundle create "*)
  git --git-dir=`+repo+` update-ref refs/backup/20260101T000000.000Z/refs/heads/main `+commit+` ;;
esac
exec git "$@"
`), 0o755)
	g := &gitexec.Git{Bin: wrapper, Env: []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}}
	snap := filepath.Join(dir, "snap")
	e, err := backupRepo(context.Background(), g, repo, snap, "acme/r")
	if err != nil {
		t.Fatal(err)
	}
	if e.Refs != 2 {
		t.Fatalf("refs = %d, want 2 (the bundle's own count)", e.Refs)
	}
	restored := filepath.Join(dir, "restored.git")
	plain := &gitexec.Git{Bin: "git", Env: g.Env}
	if err := restoreRepo(context.Background(), plain, snap, restored, *e); err != nil {
		t.Fatalf("restore: %v", err)
	}
}

func TestRestoreRejectsUnsafeLFSEntries(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.key")
	os.WriteFile(secret, []byte("master key"), 0o600)
	oid := strings.Repeat("a", 64)
	lfsDir := filepath.Join(dir, "snap.lfs", oid[0:2], oid[2:4])
	os.MkdirAll(lfsDir, 0o755)

	os.Symlink(secret, filepath.Join(lfsDir, oid))
	if _, _, err := copyObjectsBack(filepath.Join(dir, "snap.lfs"), filepath.Join(dir, "r1.git", "lfs", "objects")); err == nil {
		t.Fatal("a symlinked LFS object was restored")
	}
	os.Remove(filepath.Join(lfsDir, oid))
	os.WriteFile(filepath.Join(lfsDir, oid), []byte("not the content for this oid"), 0o644)
	if _, _, err := copyObjectsBack(filepath.Join(dir, "snap.lfs"), filepath.Join(dir, "r2.git", "lfs", "objects")); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("a corrupted LFS object was restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "r2.git", "lfs", "objects", oid[0:2], oid[2:4], oid)); err == nil {
		t.Fatal("the corrupted object was left behind")
	}
}
