package syncer

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwq-inc/gitrepo/internal/gitexec"
)

type splitRepo struct {
	t     *testing.T
	work  string
	clock int
}

func (r *splitRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.work
	r.clock++
	date := fmt.Sprintf("%d +0800", 1700000000+r.clock*60)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Dev", "GIT_AUTHOR_EMAIL=dev@example.com", "GIT_AUTHOR_DATE="+date,
		"GIT_COMMITTER_NAME=Dev", "GIT_COMMITTER_EMAIL=dev@example.com", "GIT_COMMITTER_DATE="+date)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *splitRepo) write(path, content string) {
	r.t.Helper()
	p := filepath.Join(r.work, path)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *splitRepo) commit(msg string, files map[string]string) string {
	r.t.Helper()
	for p, c := range files {
		if c == "" {
			r.git("rm", "-q", "-r", p)
		} else {
			r.write(p, c)
			r.git("add", p)
		}
	}
	r.git("commit", "-q", "-m", msg)
	return r.git("rev-parse", "HEAD")
}

func newSplitManager(t *testing.T) *Manager {
	return &Manager{git: &gitexec.Git{Bin: "git", Env: []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}, TmpDir: t.TempDir()},
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestSplitSubdirectory(t *testing.T) {
	ctx := context.Background()
	r := &splitRepo{t: t, work: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	r.commit("readme", map[string]string{"README.md": "mono\n"})
	r.commit("api: start", map[string]string{"services/api/a.txt": "a1\n"})
	r.commit("web only", map[string]string{"services/web/w.txt": "w1\n"})
	r.commit("api: change a", map[string]string{"services/api/a.txt": "a2\n"})
	r.git("checkout", "-q", "-b", "feature")
	r.commit("web on a branch", map[string]string{"services/web/w.txt": "w2\n"})
	r.commit("readme on a branch", map[string]string{"README.md": "mono 2\n"})
	r.git("checkout", "-q", "main")
	r.commit("api: add b", map[string]string{"services/api/b.txt": "b1\n"})
	r.git("merge", "-q", "--no-ff", "-m", "merge feature (no api changes)", "feature")
	r.git("checkout", "-q", "-b", "feature2")
	r.commit("api: add c on a branch", map[string]string{"services/api/c.txt": "c1\n"})
	r.git("checkout", "-q", "main")
	r.commit("api: change a again", map[string]string{"services/api/a.txt": "a3\n"})
	r.git("merge", "-q", "--no-ff", "-m", "merge feature2 (api changes)", "feature2")
	merge := r.git("rev-parse", "HEAD")
	r.git("tag", "-a", "api-v1.0", "-m", "API 1.0")
	r.commit("web: more", map[string]string{"services/web/w.txt": "w3\n"})
	mainTip := r.git("rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "hub.git")
	r.git("clone", "-q", "--bare", r.work, bare)
	tagObj := r.git("rev-parse", "refs/tags/api-v1.0")

	m := newSplitManager(t)
	hub := func(args ...string) string {
		t.Helper()
		out, err := m.git.Run(ctx, bare, args...)
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	want := map[string]string{"refs/heads/main": mainTip, "refs/tags/v1.0": tagObj, "refs/heads/feature": r.git("rev-parse", "feature")}
	got, err := m.splitRefs(ctx, bare, 7, "services/api", want)
	if err != nil {
		t.Fatal(err)
	}
	split := got["refs/heads/main"]

	// The tree is the directory's; the root holds its files.
	if hub("rev-parse", split+"^{tree}") != hub("rev-parse", mainTip+":services/api") {
		t.Fatal("split tree differs from the directory")
	}
	// Only the commits that changed the directory, newest first.
	subjects := hub("log", "--format=%s", split)
	wantSubjects := "merge feature2 (api changes)\napi: change a again\napi: add c on a branch\napi: add b\napi: change a\napi: start"
	if subjects != wantSubjects {
		t.Fatalf("split history:\n%s\nwant:\n%s", subjects, wantSubjects)
	}
	// The real merge keeps both parents; authorship and dates are kept.
	if parents := strings.Fields(hub("log", "-1", "--format=%P", split)); len(parents) != 2 {
		t.Fatalf("merge parents: %v", parents)
	}
	if hub("log", "-1", "--format=%an %ae %ad %cd", split) != hub("log", "-1", "--format=%an %ae %ad %cd", merge) {
		t.Fatal("author or dates changed")
	}
	// The web-only tip maps to the last api commit.
	if hub("rev-parse", split) != hub("rev-parse", got["refs/tags/v1.0"]+"^{commit}") {
		t.Fatal("main (after a web-only commit) and the tag on the merge should split to the same commit")
	}
	// The tag is rewritten: same message, named after the mirror tag.
	if typ := hub("cat-file", "-t", got["refs/tags/v1.0"]); typ != "tag" {
		t.Fatalf("tag type %s", typ)
	}
	tagText := hub("cat-file", "-p", got["refs/tags/v1.0"])
	if !strings.Contains(tagText, "tag v1.0\n") || !strings.Contains(tagText, "API 1.0") || !strings.Contains(tagText, "object "+split) {
		t.Fatalf("rewritten tag:\n%s", tagText)
	}
	// The branch that never touched the directory splits to where it forked.
	if hub("log", "-1", "--format=%s", got["refs/heads/feature"]) != "api: change a" {
		t.Fatal("feature split")
	}
	// The values are kept reachable for gc.
	if v := hub("rev-parse", "refs/gitrepo/split/7/refs/heads/main"); v != split {
		t.Fatalf("split ref %s", v)
	}

	// Deterministic: without the cache, the same commits.
	os.RemoveAll(filepath.Join(bare, "gitrepo"))
	again, err := newSplitManager(t).splitRefs(ctx, bare, 7, "services/api", want)
	if err != nil {
		t.Fatal(err)
	}
	for ref, v := range got {
		if again[ref] != v {
			t.Fatalf("%s: %s, then %s", ref, v, again[ref])
		}
	}

	// Incremental: the directory is removed, then added back.
	r.commit("api: removed", map[string]string{"services/api": ""})
	gone := r.git("rev-parse", "HEAD")
	r.commit("api: back", map[string]string{"services/api/new.txt": "n\n"})
	back := r.git("rev-parse", "HEAD")
	r.git("push", "-q", bare, "main")
	inc, err := m.splitRefs(ctx, bare, 7, "services/api", map[string]string{"refs/heads/main": back, "refs/heads/gone": gone})
	if err != nil {
		t.Fatal(err)
	}
	if inc["refs/heads/gone"] != split {
		t.Fatal("removing the directory should leave the mirror at its last state")
	}
	if hub("log", "-1", "--format=%s%n%P", inc["refs/heads/main"]) != "api: back\n"+split {
		t.Fatalf("re-added directory: %s", hub("log", "-1", "--format=%s %P", inc["refs/heads/main"]))
	}
	os.RemoveAll(filepath.Join(bare, "gitrepo"))
	fresh, err := newSplitManager(t).splitRefs(ctx, bare, 8, "services/api", map[string]string{"refs/heads/main": back, "refs/heads/gone": gone})
	if err != nil || fresh["refs/heads/main"] != inc["refs/heads/main"] {
		t.Fatalf("incremental %s, from scratch %s (%v)", inc["refs/heads/main"], fresh["refs/heads/main"], err)
	}
	// The old split refs of remote 7 that are no longer wanted are gone.
	if _, err := m.git.Run(ctx, bare, "rev-parse", "--verify", "--quiet", "refs/gitrepo/split/7/refs/tags/v1.0"); err == nil {
		t.Fatal("stale split ref kept")
	}

	// A directory that never existed: nothing to send.
	none, err := m.splitRefs(ctx, bare, 9, "nowhere", map[string]string{"refs/heads/main": back})
	if err != nil || len(none) != 0 {
		t.Fatalf("missing directory: %v %v", none, err)
	}

	// Forgetting removes the refs and the cache.
	if err := m.ForgetSplits(ctx, bare, 0); err != nil {
		t.Fatal(err)
	}
	if refs, _ := m.git.ForEachRef(ctx, bare, "refs/gitrepo/split/"); len(refs) != 0 {
		t.Fatalf("split refs left: %v", refs)
	}
	if files, _ := filepath.Glob(filepath.Join(bare, "gitrepo", "split", "*")); len(files) != 0 {
		t.Fatalf("caches left: %v", files)
	}
}

// A corrupt cache is rebuilt, giving the same result.
func TestSplitCorruptCache(t *testing.T) {
	ctx := context.Background()
	r := &splitRepo{t: t, work: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	r.commit("one", map[string]string{"lib/x.txt": "1\n"})
	r.commit("two", map[string]string{"lib/x.txt": "2\n"})
	tip := r.git("rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "hub.git")
	r.git("clone", "-q", "--bare", r.work, bare)
	m := newSplitManager(t)
	first, err := m.splitRefs(ctx, bare, 1, "lib", map[string]string{"refs/heads/main": tip})
	if err != nil {
		t.Fatal(err)
	}
	// A cache that claims a commit was split, but without its details.
	path := m.splitCachePath(bare, 1)
	os.WriteFile(path, []byte(splitCacheHeader+"\nprefix lib\nC "+r.git("rev-parse", "HEAD~1")+" deadbeef\n"), 0o644)
	r.commit("three", map[string]string{"lib/x.txt": "3\n"})
	r.git("push", "-q", bare, "main")
	tip3 := r.git("rev-parse", "HEAD")
	got, err := m.splitRefs(ctx, bare, 1, "lib", map[string]string{"refs/heads/main": tip3})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := m.git.Run(ctx, bare, "log", "--format=%s", got["refs/heads/main"])
	if strings.TrimSpace(string(out)) != "three\ntwo\none" {
		t.Fatalf("after rebuilding: %s", out)
	}
	parent, _ := m.git.Run(ctx, bare, "rev-parse", got["refs/heads/main"]+"^")
	if strings.TrimSpace(string(parent)) != first["refs/heads/main"] {
		t.Fatal("rebuilt history differs from the first one")
	}
}

// The split is the same, commit for commit, as `git subtree split` makes:
// a mirror split by Gitrepo and one split by hand share their history.
func TestSplitMatchesGitSubtree(t *testing.T) {
	if out, err := exec.Command("git", "subtree", "-h").CombinedOutput(); err != nil && !strings.Contains(string(out), "usage: git subtree") {
		t.Skip("git subtree is not installed")
	}
	ctx := context.Background()
	r := &splitRepo{t: t, work: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	r.commit("readme", map[string]string{"README.md": "mono\n"})
	r.commit("api: start", map[string]string{"services/api/a.txt": "a1\n"})
	r.commit("web only", map[string]string{"services/web/w.txt": "w1\n"})
	r.commit("api: change a", map[string]string{"services/api/a.txt": "a2\n"})
	r.git("checkout", "-q", "-b", "feature")
	r.commit("web on a branch", map[string]string{"services/web/w.txt": "w2\n"})
	r.git("checkout", "-q", "main")
	r.commit("api: add b", map[string]string{"services/api/b.txt": "b1\n"})
	r.git("merge", "-q", "--no-ff", "-m", "merge feature", "feature")
	r.git("checkout", "-q", "-b", "feature2")
	r.commit("api: add c on a branch", map[string]string{"services/api/c.txt": "c1\n"})
	r.git("checkout", "-q", "main")
	r.commit("api: change a again", map[string]string{"services/api/a.txt": "a3\n"})
	r.git("merge", "-q", "--no-ff", "-m", "merge feature2\n\nwith a body", "feature2")
	tip := r.git("rev-parse", "HEAD")
	theirs := r.git("subtree", "split", "--prefix=services/api", "-q", "main")
	bare := filepath.Join(t.TempDir(), "hub.git")
	r.git("clone", "-q", "--bare", r.work, bare)
	got, err := newSplitManager(t).splitRefs(ctx, bare, 1, "services/api", map[string]string{"refs/heads/main": tip})
	if err != nil {
		t.Fatal(err)
	}
	if got["refs/heads/main"] != theirs {
		t.Fatalf("split %s, git subtree split %s", got["refs/heads/main"], theirs)
	}
}
