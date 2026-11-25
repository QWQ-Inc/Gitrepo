package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionsExpire(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	p := &OIDCProvider{Name: "se", Kind: "stackenigma", Issuer: "https://id.example.com", ClientID: "c", Allow: []string{"*@example.com", "group:ops"}, Enabled: true}
	if err := s.CreateOIDCProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetOIDCProvider(ctx, "se"); err != nil || len(got.Allow) != 2 || got.Allow[1] != "group:ops" {
		t.Fatalf("%+v %v", got, err)
	}
	live := &Session{Hash: "live", Provider: "se", Subject: "u1", Name: "sso:se:a", Scopes: []string{"admin"}, ExpiresAt: FromTime(time.Now().Add(time.Hour))}
	dead := &Session{Hash: "dead", Provider: "se", Subject: "u2", Name: "sso:se:b", Scopes: []string{"admin"}, ExpiresAt: FromTime(time.Now().Add(-time.Second))}
	for _, x := range []*Session{live, dead} {
		if err := s.CreateSession(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.GetSessionByHash(ctx, "dead"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an expired session resolves: %v", err)
	}
	if x, err := s.GetSessionByHash(ctx, "live"); err != nil || x.Subject != "u1" {
		t.Fatalf("%+v %v", x, err)
	}
	if list, _ := s.ListSessions(ctx); len(list) != 1 {
		t.Fatalf("listed: %d", len(list))
	}
	if err := s.PruneSessions(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d sessions after pruning", n)
	}
	// Deleting the provider signs everyone out.
	if err := s.DeleteOIDCProvider(ctx, "se"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSessionByHash(ctx, "live"); !errors.Is(err, ErrNotFound) {
		t.Fatal("a session outlived its provider")
	}
}
