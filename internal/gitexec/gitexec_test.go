package gitexec

import (
	"context"
	"path/filepath"
	"testing"
)

func TestParsePorcelain(t *testing.T) {
	out := "To ../remote.git\n" +
		"!\tabc:refs/heads/main\t[rejected] (atomic push failed)\n" +
		"!\tabc:refs/heads/x\t[rejected] (stale info)\n" +
		"*\tabc:refs/heads/new\t[new branch]\n" +
		"+\tdef:refs/heads/f\ta9a7711...0f2273d (forced update)\n" +
		"-\t:refs/heads/gone\t[deleted]\n" +
		"=\tabc:refs/tags/v1\t[up to date]\n" +
		"!\tabc:refs/heads/p\t[remote rejected] (pre-receive hook declined)\n" +
		"Done\n"
	got := ParsePorcelain(out)
	if len(got) != 7 {
		t.Fatalf("parsed %d refs: %+v", len(got), got)
	}
	if !got["refs/heads/main"].AtomicCollateral() {
		t.Error("main should be atomic collateral")
	}
	if !got["refs/heads/x"].StaleLease() {
		t.Error("x should be a stale lease")
	}
	for _, ok := range []string{"refs/heads/new", "refs/heads/f", "refs/heads/gone", "refs/tags/v1"} {
		if !got[ok].OK() {
			t.Errorf("%s should be ok: %+v", ok, got[ok])
		}
	}
	p := got["refs/heads/p"]
	if p.OK() || p.StaleLease() || p.Summary != "[remote rejected]" || p.Reason != "pre-receive hook declined" {
		t.Errorf("unexpected status for p: %+v", p)
	}
	if got["refs/heads/f"].Flag != '+' {
		t.Error("f should be forced")
	}
}

func TestTickets(t *testing.T) {
	tk := NewTickets()
	id, release := tk.Issue(Cred{Username: "u", Secret: "s"})
	if a, ok := tk.Answer(id, "Username for 'https://example.com': "); !ok || a != "u" {
		t.Fatalf("username answer %q %v", a, ok)
	}
	if a, ok := tk.Answer(id, "Password for 'https://u@example.com': "); !ok || a != "s" {
		t.Fatalf("password answer %q %v", a, ok)
	}
	release()
	if _, ok := tk.Answer(id, "Password: "); ok {
		t.Fatal("released ticket still answers")
	}
}

func TestIsZero(t *testing.T) {
	if !IsZero(ZeroSHA) || !IsZero("") || IsZero("0f2273dfb913c24efc16d09973ee2506536cd106") {
		t.Fatal("IsZero misbehaves")
	}
}

// TestPushLeaseSemantics pins down the git behaviour the sync engine relies
// on: an empty lease means "must not exist", a matching lease permits a
// non-fast-forward update without '+', and a stale lease is reported per ref.
func TestPushLeaseSemantics(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	g := &Git{Bin: "git", Env: []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com"}}
	hub, remote := filepath.Join(dir, "hub.git"), filepath.Join(dir, "remote.git")
	for _, p := range []string{hub, remote} {
		if err := g.InitBare(ctx, p, "main"); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(parent, msg string) string {
		args := []string{"commit-tree", "4b825dc642cb6eb9a060e54bf8d69288fbee4904", "-m", msg}
		if parent != "" {
			args = append(args, "-p", parent)
		}
		out, err := g.Run(ctx, hub, args...)
		if err != nil {
			t.Fatal(err)
		}
		return string(out[:40])
	}
	a := commit("", "a")
	b := commit(a, "b")
	x := commit("", "x") // unrelated to a/b
	if err := g.UpdateRefs(ctx, hub, []string{"update refs/heads/keep " + a, "update refs/heads/keep2 " + b, "update refs/heads/keep3 " + x}); err != nil {
		t.Fatal(err)
	}
	url := remote

	res, err := g.Push(ctx, hub, url, nil, []PushOp{{Ref: "refs/heads/main", New: a}}, true)
	if err != nil || !res.Refs["refs/heads/main"].OK() {
		t.Fatalf("create with empty lease: %v %+v", err, res)
	}
	res, err = g.Push(ctx, hub, url, nil, []PushOp{{Ref: "refs/heads/main", New: b}}, true)
	if err != nil || !res.Refs["refs/heads/main"].StaleLease() {
		t.Fatalf("empty lease on existing ref should be stale: %v %+v", err, res)
	}
	res, err = g.Push(ctx, hub, url, nil, []PushOp{{Ref: "refs/heads/main", New: x, Expect: a}}, true)
	if err != nil || res.Refs["refs/heads/main"].Flag != '+' {
		t.Fatalf("non-ff with matching lease should force: %v %+v", err, res)
	}
	res, err = g.Push(ctx, hub, url, nil, []PushOp{
		{Ref: "refs/heads/main", New: b, Expect: a}, // stale: remote is x
		{Ref: "refs/heads/other", New: b},
	}, true)
	if err != nil || !res.Refs["refs/heads/main"].StaleLease() || !res.Refs["refs/heads/other"].AtomicCollateral() {
		t.Fatalf("atomic push with one stale ref: %v %+v", err, res)
	}
	refs, err := g.LsRemote(ctx, url, nil)
	if err != nil || refs["refs/heads/main"] != x || refs["refs/heads/other"] != "" {
		t.Fatalf("remote state after rejected atomic push: %v %v", refs, err)
	}
	if res, err = g.Push(ctx, hub, url, nil, []PushOp{{Ref: "refs/heads/other", New: b}}, true); err != nil || !res.Refs["refs/heads/other"].OK() {
		t.Fatalf("create other: %v %+v", err, res)
	}
	res, err = g.Push(ctx, hub, url, nil, []PushOp{{Ref: "refs/heads/other", Expect: b}}, true)
	if err != nil || res.Refs["refs/heads/other"].Flag != '-' {
		t.Fatalf("delete with lease: %v %+v", err, res)
	}
	if !g.IsAncestor(ctx, hub, a, b) || g.IsAncestor(ctx, hub, b, a) || g.IsAncestor(ctx, hub, a, x) {
		t.Fatal("IsAncestor misbehaves")
	}
	if g.IsAncestor(ctx, hub, "1111111111111111111111111111111111111111", b) {
		t.Fatal("IsAncestor with a missing object must be false")
	}
}
