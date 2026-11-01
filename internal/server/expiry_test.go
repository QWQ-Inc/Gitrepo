package server_test

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/client"
	"github.com/qwq-inc/gitrepo/internal/server"
)

// GitHub and GitLab report when a token expires; Gitrepo reads it so that
// expiry warnings work without anyone typing in a date. A date set by hand
// always wins.
func TestCredentialExpiryDetection(t *testing.T) {
	e := newEnv(t)
	fake := newFakePlatforms(t)
	type target struct {
		Name      string     `json:"name"`
		ExpiresAt *time.Time `json:"expires_at"`
		Detected  bool       `json:"expires_detected"`
	}
	day := func(s string) time.Time { d, _ := time.Parse("2006-01-02", s); return d }
	expect := func(what string, got target, date string, detected bool) {
		t.Helper()
		if date == "" {
			if got.ExpiresAt != nil || got.Detected {
				t.Fatalf("%s: expiry %v (detected %v), want none", what, got.ExpiresAt, got.Detected)
			}
			return
		}
		if got.ExpiresAt == nil || !got.ExpiresAt.Equal(day(date)) || got.Detected != detected {
			t.Fatalf("%s: expiry %v (detected %v), want %s (detected %v)", what, got.ExpiresAt, got.Detected, date, detected)
		}
	}
	get := func(name string) target {
		var ts []target
		e.do(http.MethodGet, "/api/targets", nil, &ts)
		for _, x := range ts {
			if x.Name == name {
				return x
			}
		}
		t.Fatalf("no target %s", name)
		return target{}
	}
	patch := func(name string, body map[string]any) target {
		var out target // fresh: absent fields must read as unset
		e.do(http.MethodPatch, "/api/targets/"+name, body, &out)
		return out
	}
	check := func(name string) target {
		var res struct {
			OK        bool       `json:"ok"`
			Error     string     `json:"error"`
			ExpiresAt *time.Time `json:"expires_at"`
			Detected  bool       `json:"expires_detected"`
		}
		e.do(http.MethodPost, "/api/targets/"+name+"/check", nil, &res)
		if !res.OK {
			t.Fatalf("check %s: %s", name, res.Error)
		}
		return target{Name: name, ExpiresAt: res.ExpiresAt, Detected: res.Detected}
	}

	fake.setTokenExpiry("2026-12-01")
	var gh, gl target
	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "github", "provider": "github", "namespace": "acme", "base_url": fake.srv.URL + "/gh",
		"credential": map[string]any{"kind": "token", "secret": ghToken},
	}, &gh)
	expect("new GitHub target", gh, "2026-12-01", true)
	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "gitlab", "provider": "gitlab", "namespace": "acme-group", "base_url": fake.srv.URL + "/gl",
		"credential": map[string]any{"kind": "token", "secret": glToken},
	}, &gl)
	expect("new GitLab target", gl, "2026-12-01", true)

	// The token is extended on the platform: a check reads it again.
	fake.setTokenExpiry("2027-03-01")
	expect("check after the token was extended", check("github"), "2027-03-01", true)
	expect("stored after the check", get("github"), "2027-03-01", true)

	// Set by hand: kept, whatever the platform says.
	gh = patch("github", map[string]any{"expires_at": "2026-06-30"})
	expect("set by hand", gh, "2026-06-30", false)
	expect("check keeps a date set by hand", check("github"), "2026-06-30", false)
	// Unrelated changes keep it set by hand too.
	gh = patch("github", map[string]any{"namespace": "acme"})
	expect("unrelated change", gh, "2026-06-30", false)

	// Cleared: read from the platform again.
	gh = patch("github", map[string]any{"expires_at": ""})
	expect("cleared", gh, "2027-03-01", true)

	// A token without an expiry.
	fake.setTokenExpiry("")
	expect("check of a token that does not expire", check("gitlab"), "", false)
	expect("stored", get("gitlab"), "", false)

	// A new secret: the old expiry (even one set by hand) belonged to the
	// old token, and the new token's is read.
	patch("gitlab", map[string]any{"expires_at": "2026-06-30"})
	fake.setTokenExpiry("2027-05-05")
	gl = patch("gitlab", map[string]any{"credential": map[string]any{"kind": "token", "secret": glToken}})
	expect("new secret", gl, "2027-05-05", true)
	// A new secret with a date given in the same request uses that date.
	gl = patch("gitlab", map[string]any{"expires_at": "2026-08-08", "credential": map[string]any{"kind": "token", "secret": glToken}})
	expect("new secret with a date", gl, "2026-08-08", false)

	// The patrol reads expiries again (daily, and on its first run after a
	// start): a target stored while its platform reported nothing picks up
	// an expiry without anyone running a check.
	fake.setTokenExpiry("")
	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "later", "provider": "gitlab", "namespace": "acme-group", "base_url": fake.srv.URL + "/gl",
		"credential": map[string]any{"kind": "token", "secret": glToken},
	}, nil)
	expect("stored without an expiry", get("later"), "", false)
	fake.setTokenExpiry("2027-09-09")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	e.srv.Shutdown(ctx)
	cancel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := *e.cfg
	cfg.PublicURL = "http://" + ln.Addr().String()
	srv, err := server.New(&cfg, slog.New(slog.NewTextHandler(e.logs, nil)), server.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(ln); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	e.api = client.New(cfg.PublicURL, adminToken)
	eventually(t, "expiry read by the patrol", 15*time.Second, func() bool {
		x := get("later")
		return x.ExpiresAt != nil && x.ExpiresAt.Equal(day("2027-09-09")) && x.Detected
	})
	expect("a date set by hand is not touched by the patrol", get("gitlab"), "2026-08-08", false)
}
