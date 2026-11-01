package server_test

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/qwq-inc/gitrepo/internal/client"
)

func TestRepoVisibilityChange(t *testing.T) {
	e := newEnv(t)
	fake := newFakePlatforms(t)
	setupFourPlatforms(t, e, fake)
	// Gitee mirrors are always private here: that target keeps its setting.
	e.do(http.MethodPatch, "/api/targets/gitee", map[string]any{"visibility": "private"}, nil)
	var dev client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	e.do(http.MethodPost, "/api/repos", map[string]any{"slug": "acme/svc", "visibility": "public"}, nil)
	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	e.commit(work, "a.txt", "1\n")
	e.run(work, "push", "-q", e.hubURL(dev.Token, "acme/svc"), "main")
	anon := "http://" + e.hubAddr + "/acme/svc.git"
	if _, err := gitCmd("", "ls-remote", anon); err != nil {
		t.Fatalf("setup: public repository not readable anonymously: %v", err)
	}

	var res struct {
		Repo struct {
			Visibility string `json:"visibility"`
		} `json:"repo"`
		Mirrors []struct {
			Mirror, State, Detail string
		} `json:"mirrors"`
	}
	e.do(http.MethodPatch, "/api/repos/acme/svc", map[string]any{"visibility": "private"}, &res)
	if res.Repo.Visibility != "private" {
		t.Fatalf("hub visibility: %+v", res)
	}
	states := map[string]string{}
	for _, m := range res.Mirrors {
		states[m.Mirror] = m.State
	}
	want := map[string]string{"github:acme/svc": "changed", "gitlab:acme-group/svc": "changed", "forgejo:acme/svc": "changed", "gitee:acme/svc": "kept"}
	for k, v := range want {
		if states[k] != v {
			t.Errorf("%s: %s, want %s (all: %+v)", k, states[k], v, res.Mirrors)
		}
	}
	for _, c := range []string{"gh private acme/svc=true", "gl visibility acme-group/svc=private", "gt private acme/svc=true"} {
		if !fake.called(c) {
			t.Errorf("platform call missing: %s (calls %v)", c, fake.calls)
		}
	}
	if fake.called("ge private acme/svc=true name=svc") {
		t.Error("a target with a fixed visibility was changed")
	}
	if _, err := gitCmd("", "ls-remote", anon); err == nil {
		t.Fatal("the repository is still readable anonymously after going private")
	}
	if err := e.api.Do(t.Context(), http.MethodPatch, "/api/repos/acme/svc", map[string]any{"visibility": "secret"}, nil); err == nil {
		t.Fatal("an invalid visibility was accepted")
	}
	var page auditPage
	e.do(http.MethodGet, "/api/audit?repo=acme/svc&action=PATCH", nil, &page)
	if len(page.Entries) != 2 || page.Entries[1].Detail != "visibility public → private (mirrors: 3 changed, 0 not changed)" || page.Entries[0].Status != 400 {
		t.Fatalf("audit: %+v", page.Entries)
	}
}
