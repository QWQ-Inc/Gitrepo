package bundles

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/gitexec"
)

var testEnv = []string{
	"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
	"GIT_AUTHOR_NAME=Dev", "GIT_AUTHOR_EMAIL=dev@example.com",
	"GIT_COMMITTER_NAME=Dev", "GIT_COMMITTER_EMAIL=dev@example.com",
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), testEnv...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// Removing a repository's bundles (post-receive after a ref was deleted or
// rewritten) must not wait for a slow bundle build, and the build must not
// install its now outdated bundle afterwards.
func TestRemoveDuringBuild(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	git(t, dir, "init", "-q", "-b", "main", work)
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("1\n"), 0o644)
	git(t, work, "add", "a.txt")
	git(t, work, "commit", "-q", "-m", "one")
	repo := filepath.Join(dir, "demo.git")
	git(t, dir, "clone", "-q", "--bare", work, repo)

	started := filepath.Join(dir, "started")
	wrapper := filepath.Join(dir, "git-slow")
	os.WriteFile(wrapper, []byte("#!/bin/sh\ncase \" $* \" in *\" bundle create \"*) touch "+started+"; sleep 3 ;; esac\nexec git \"$@\"\n"), 0o755)

	m := &Manager{Dir: filepath.Join(dir, "bundles"), Git: &gitexec.Git{Bin: wrapper, Env: testEnv}}
	type result struct {
		ok  bool
		err error
	}
	done := make(chan result, 1)
	go func() {
		ok, err := m.Refresh(context.Background(), "acme/demo", 1, repo, true)
		done <- result{ok, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the bundle build did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}

	start := time.Now()
	if err := m.Remove("acme/demo"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Remove waited %s for the bundle build", d)
	}
	res := <-done
	if res.ok || res.err != nil {
		t.Fatalf("the build that started before the removal: installed=%v err=%v (want discarded quietly)", res.ok, res.err)
	}
	if b, ok := m.Current("acme/demo", 1); ok {
		t.Fatalf("an outdated bundle was installed after the removal: %+v", b)
	}

	m.Git.Bin = "git"
	if ok, err := m.Refresh(context.Background(), "acme/demo", 1, repo, true); !ok || err != nil {
		t.Fatalf("a later build: installed=%v err=%v", ok, err)
	}
	if _, ok := m.Current("acme/demo", 1); !ok {
		t.Fatal("no bundle after a later build")
	}
}
