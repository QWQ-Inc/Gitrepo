package provider

import (
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
