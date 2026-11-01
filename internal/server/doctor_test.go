package server_test

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/client"
)

func TestDoctor(t *testing.T) {
	e := newEnv(t)
	fake := newFakePlatforms(t)
	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "github", "provider": "github", "namespace": "acme", "base_url": fake.srv.URL + "/gh",
		"credential": map[string]any{"kind": "token", "secret": ghToken},
	}, nil)
	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "stale", "provider": "github", "namespace": "acme", "base_url": fake.srv.URL + "/gh",
		"credential": map[string]any{"kind": "token", "secret": "revoked-token"}, "expires_at": "2020-01-01",
	}, nil)
	var edgeTok client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "edge-sh", "scopes": []string{"edge"}}, &edgeTok)
	n := startEdgeNode(t, e, edgeTok.Token, filepath.Join(t.TempDir(), "edge"), "", "")
	_ = n
	time.Sleep(300 * time.Millisecond)

	var res struct {
		OK     bool `json:"ok"`
		Checks []struct {
			Name, Status, Detail string
		} `json:"checks"`
	}
	e.do(http.MethodGet, "/api/doctor", nil, &res)
	got := map[string]string{}
	for _, c := range res.Checks {
		got[c.Name] = c.Status + ": " + c.Detail
	}
	for name, want := range map[string]string{
		"git":                 "ok: git 2.",
		"public url":          "ok: ",
		"disk":                "",
		"target github":       "ok: github as octo",
		"target stale":        "fail: github rejected the credential",
		"target stale expiry": "fail: credential expired on 2020-01-01",
		"edge edge-sh":        "ok: following",
	} {
		if !strings.HasPrefix(got[name], want) || got[name] == "" {
			t.Errorf("%s: %q, want prefix %q", name, got[name], want)
		}
	}
	if res.OK {
		t.Fatal("doctor reported ok with a failing credential")
	}
	if res.Checks[0].Status != "fail" {
		t.Fatalf("failures must come first: %+v", res.Checks[0])
	}
}
