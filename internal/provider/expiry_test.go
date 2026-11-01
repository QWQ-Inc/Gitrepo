package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/store"
)

func TestGitHubTokenExpiry(t *testing.T) {
	header := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/user" || r.Header.Get("Authorization") != "Bearer pat" {
			w.WriteHeader(401)
			return
		}
		if header != "" {
			w.Header().Set("GitHub-Authentication-Token-Expiration", header)
		}
		fmt.Fprint(w, `{"login":"octo"}`)
	}))
	defer srv.Close()
	g := &GitHub{}
	tg := &store.Target{Provider: "github", BaseURL: srv.URL, CredKind: "token"}
	ctx := context.Background()

	for _, c := range []struct {
		header string
		want   time.Time
	}{
		{"2026-12-01 00:00:00 UTC", time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)},
		{"2026-12-01 10:00:00 -0700", time.Date(2026, 12, 1, 17, 0, 0, 0, time.UTC)},
	} {
		header = c.header
		exp, known, err := g.TokenExpiry(ctx, tg, "pat")
		if err != nil || !known || !exp.Equal(c.want) {
			t.Errorf("header %q: %v %v %v, want %v", c.header, exp, known, err, c.want)
		}
	}
	header = ""
	if exp, known, err := g.TokenExpiry(ctx, tg, "pat"); err != nil || !known || !exp.IsZero() {
		t.Errorf("token without expiry: %v %v %v", exp, known, err)
	}
	header = "next tuesday"
	if _, known, err := g.TokenExpiry(ctx, tg, "pat"); err == nil || known {
		t.Errorf("unparseable header accepted: %v %v", known, err)
	}
	if _, _, err := g.TokenExpiry(ctx, tg, "wrong"); err == nil {
		t.Error("a rejected token must be an error")
	}
	app := *tg
	app.CredKind = CredGitHubApp
	if _, known, err := g.TokenExpiry(ctx, &app, "ghs_x"); err != nil || known {
		t.Errorf("GitHub App tokens have no expiry to report: %v %v", known, err)
	}
}

func TestGitLabTokenExpiry(t *testing.T) {
	body, status := "", 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/personal_access_tokens/self" || r.Header.Get("PRIVATE-TOKEN") != "glpat" {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	g := &GitLab{}
	tg := &store.Target{Provider: "gitlab", BaseURL: srv.URL, CredKind: "token"}
	ctx := context.Background()

	body = `{"id":1,"name":"mirror","expires_at":"2027-01-31","active":true}`
	if exp, known, err := g.TokenExpiry(ctx, tg, "glpat"); err != nil || !known || !exp.Equal(time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("expiring token: %v %v %v", exp, known, err)
	}
	body = `{"id":1,"name":"mirror","expires_at":null}`
	if exp, known, err := g.TokenExpiry(ctx, tg, "glpat"); err != nil || !known || !exp.IsZero() {
		t.Errorf("token without expiry: %v %v %v", exp, known, err)
	}
	body, status = `{"message":"404 Not Found"}`, 404 // GitLab before 15.5
	if _, known, err := g.TokenExpiry(ctx, tg, "glpat"); err != nil || known {
		t.Errorf("old GitLab: %v %v", known, err)
	}
	if _, _, err := g.TokenExpiry(ctx, tg, "wrong"); err == nil {
		t.Error("a rejected token must be an error")
	}
}
