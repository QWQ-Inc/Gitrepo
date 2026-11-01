package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/qwq-inc/gitrepo/internal/store"
)

func TestRepoURLs(t *testing.T) {
	cases := []struct {
		kind string
		t    store.Target
		ns   string
		want string
	}{
		{"github", store.Target{Namespace: "acme"}, "", "https://github.com/acme/demo.git"},
		{"github", store.Target{Namespace: "acme", BaseURL: "https://ghe.example.com/"}, "team", "https://ghe.example.com/team/demo.git"},
		{"gitlab", store.Target{Namespace: "group/sub"}, "", "https://gitlab.com/group/sub/demo.git"},
		{"generic", store.Target{URLTemplate: "ssh://git@git.example.com/{namespace}/{name}.git", Namespace: "backup"}, "", "ssh://git@git.example.com/backup/demo.git"},
		{"generic", store.Target{URLTemplate: "https://git.example.com/{namespace}/{name}.git"}, "", "https://git.example.com/demo.git"},
		{"generic", store.Target{BaseURL: "https://git.example.com"}, "ns", "https://git.example.com/ns/demo.git"},
		{"gitea", store.Target{BaseURL: "https://forgejo.example.com/", Namespace: "acme"}, "", "https://forgejo.example.com/acme/demo.git"},
		{"gitee", store.Target{Namespace: "acme"}, "", "https://gitee.com/acme/demo.git"},
	}
	for _, c := range cases {
		p, _ := Get(c.kind)
		got, err := p.RepoURL(&c.t, c.ns, "demo")
		if err != nil || got != c.want {
			t.Errorf("%s %+v: got %q (%v), want %q", c.kind, c.t, got, err, c.want)
		}
	}
}

func TestGitHubAPIURL(t *testing.T) {
	g := &GitHub{}
	if u := g.api(&store.Target{}); u != "https://api.github.com" {
		t.Errorf("github.com api = %s", u)
	}
	if u := g.api(&store.Target{BaseURL: "https://ghe.example.com"}); u != "https://ghe.example.com/api/v3" {
		t.Errorf("GHE api = %s", u)
	}
}

func TestGitLabRuleMatching(t *testing.T) {
	if !matchesAny("main", []string{"dev", "main"}) || matchesAny("main", []string{"maintenance"}) {
		t.Error("exact match")
	}
	if !matchesAny("release/*", []string{"release/1.0"}) || !matchesAny("*-stable", []string{"16-stable"}) || matchesAny("release/*", []string{"main"}) {
		t.Error("wildcard match")
	}
	if matchesAny("main", nil) {
		t.Error("empty list matched")
	}
}

func TestValidate(t *testing.T) {
	gh, _ := Get("github")
	if err := gh.Validate(&store.Target{CredKind: "token"}); err == nil {
		t.Error("github without namespace accepted")
	}
	gen, _ := Get("generic")
	if err := gen.Validate(&store.Target{URLTemplate: "https://x/{namespace}.git", CredKind: "none"}); err == nil {
		t.Error("template without {name} accepted")
	}
}

// Transport errors must not echo URLs, which can carry access tokens.
func TestErrorsDoNotLeakTokens(t *testing.T) {
	_, err := doJSON(context.Background(), "GET", "http://127.0.0.1:1/api/v5/user?access_token=SECRET123", nil, nil, nil)
	if err == nil || strings.Contains(err.Error(), "SECRET123") {
		t.Fatalf("error leaks the token: %v", err)
	}
}

func TestGiteeErrorsDoNotLeakTheToken(t *testing.T) {
	g := &Gitee{}
	tgt := &store.Target{BaseURL: "https://gitee.example.com:8443x", Namespace: "acme", CredKind: "token"}
	_, err := g.Check(context.Background(), tgt, "SECRET-GITEE-TOKEN")
	if err == nil || strings.Contains(err.Error(), "SECRET-GITEE-TOKEN") {
		t.Fatalf("error leaks the token: %v", err)
	}
	if got := redactURL("https://u:p@host:99x/a?access_token=X"); strings.Contains(got, "access_token") || strings.Contains(got, "u:p") {
		t.Fatalf("redactURL kept secrets: %s", got)
	}
}

func TestLFSLimits(t *testing.T) {
	gh, _ := Get("github")
	if l, ok := gh.(LFSLimiter); !ok || l.MaxLFSObjectSize() != 2_000_000_000 {
		t.Fatal("GitHub's LFS limit is not declared")
	}
	gl, _ := Get("gitlab")
	if _, ok := gl.(LFSLimiter); ok {
		t.Fatal("GitLab's LFS limit depends on the instance and must not be assumed")
	}
}

func TestGitHubUploadTarget(t *testing.T) {
	g := &GitHub{}
	dotcom := &store.Target{}
	ghes := &store.Target{BaseURL: "https://git.corp.example"}
	for _, c := range []struct {
		t    *store.Target
		raw  string
		want string
	}{
		{dotcom, "https://uploads.github.com/repos/a/b/releases/1/assets{?name,label}", "https://uploads.github.com/repos/a/b/releases/1/assets"},
		{dotcom, "https://api.github.com/repos/a/b/releases/1/assets", "https://api.github.com/repos/a/b/releases/1/assets"},
		{dotcom, "https://evil.example/repos/a/b/releases/1/assets{?name}", ""},
		{dotcom, "http://uploads.github.com/x", ""},
		{ghes, "https://git.corp.example/api/uploads/repos/a/b/releases/1/assets{?name,label}", "https://git.corp.example/api/uploads/repos/a/b/releases/1/assets"},
		{ghes, "https://uploads.github.com/repos/a/b/releases/1/assets", ""},
		{ghes, "", ""},
	} {
		got, err := g.uploadTarget(c.t, c.raw)
		if c.want == "" && err == nil || c.want != "" && got != c.want {
			t.Errorf("uploadTarget(%q) = %q, %v; want %q", c.raw, got, err, c.want)
		}
	}
}
