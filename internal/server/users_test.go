package server_test

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/client"
)

type sessionRes struct {
	Token     string    `json:"token"`
	Name      string    `json:"name"`
	ExpiresAt time.Time `json:"expires_at"`
}

func statusOf(err error) int {
	var ae *client.Error
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

func TestAccounts(t *testing.T) {
	e := newEnv(t)
	anon := client.New(e.cfg.PublicURL, "")
	ctx := t.Context()

	var st struct {
		NeedsSetup bool `json:"needs_setup"`
		AdminToken bool `json:"admin_token"`
	}
	if err := anon.Do(ctx, http.MethodGet, "/api/auth/status", nil, &st); err != nil || !st.NeedsSetup || !st.AdminToken {
		t.Fatalf("status before setup: %+v %v", st, err)
	}
	// The first account needs the bootstrap admin token.
	if err := anon.Do(ctx, http.MethodPost, "/api/auth/setup", map[string]string{"admin_token": "wrong", "username": "root", "password": "correct horse"}, nil); statusOf(err) != 401 {
		t.Fatalf("setup with a wrong token: %v", err)
	}
	if err := anon.Do(ctx, http.MethodPost, "/api/auth/setup", map[string]string{"admin_token": adminToken, "username": "root", "password": "short"}, nil); statusOf(err) != 400 {
		t.Fatalf("a short password: %v", err)
	}
	var first sessionRes
	if err := anon.Do(ctx, http.MethodPost, "/api/auth/setup", map[string]string{"admin_token": adminToken, "username": "Root", "password": "correct horse"}, &first); err != nil {
		t.Fatal(err)
	}
	if first.Name != "user:root" || !strings.HasPrefix(first.Token, "grs_") {
		t.Fatalf("setup: %+v", first)
	}
	// Only once.
	if err := anon.Do(ctx, http.MethodPost, "/api/auth/setup", map[string]string{"admin_token": adminToken, "username": "eve", "password": "correct horse"}, nil); statusOf(err) != 409 {
		t.Fatalf("second setup: %v", err)
	}
	if anon.Do(ctx, http.MethodGet, "/api/auth/status", nil, &st); st.NeedsSetup {
		t.Fatal("still needs setup")
	}

	// The first account is an administrator.
	root := client.New(e.cfg.PublicURL, first.Token)
	var me struct {
		Kind     string `json:"kind"`
		Username string `json:"username"`
		Admin    bool   `json:"admin"`
	}
	if err := root.Do(ctx, http.MethodGet, "/api/me", nil, &me); err != nil || me.Kind != "user" || me.Username != "root" || !me.Admin {
		t.Fatalf("me: %+v %v", me, err)
	}
	if err := root.Do(ctx, http.MethodGet, "/api/targets", nil, nil); err != nil {
		t.Fatalf("the administrator cannot use the API: %v", err)
	}

	// Sign in with the password.
	login := func(user, pw string) (*client.Client, error) {
		var res sessionRes
		if err := anon.Do(ctx, http.MethodPost, "/api/auth/login", map[string]string{"username": user, "password": pw}, &res); err != nil {
			return nil, err
		}
		return client.New(e.cfg.PublicURL, res.Token), nil
	}
	if _, err := login("root", "wrong password"); statusOf(err) != 401 {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := login("nobody", "wrong password"); statusOf(err) != 401 {
		t.Fatalf("unknown user: %v", err)
	}
	if _, err := login("ROOT", "correct horse"); err != nil {
		t.Fatalf("login: %v", err)
	}

	// The password is also a git credential, with the account's scopes.
	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	e.commit(work, "a.txt", "1\n")
	gitURL := func(user, pw, slug string) string {
		return "http://" + user + ":" + pw + "@" + e.hubAddr + "/" + slug + ".git"
	}
	e.run(work, "push", "-q", gitURL("root", "correct%20horse", "acme/app"), "main")

	// Another account: read-only, limited to acme/*.
	var dev struct {
		Username string   `json:"username"`
		Admin    bool     `json:"admin"`
		Scopes   []string `json:"scopes"`
	}
	if err := root.Do(ctx, http.MethodPost, "/api/users", map[string]any{"username": "dev", "password": "dev password 1", "scopes": []string{"read"}, "repos": []string{"acme/*"}}, &dev); err != nil || dev.Admin {
		t.Fatalf("create dev: %+v %v", dev, err)
	}
	if _, err := gitCmd("", "ls-remote", gitURL("dev", "dev%20password%201", "acme/app")); err != nil {
		t.Fatalf("dev cannot read: %v", err)
	}
	if out, err := gitCmd(work, "push", gitURL("dev", "dev%20password%201", "acme/app"), "main:refs/heads/x"); err == nil {
		t.Fatalf("a read-only account pushed: %s", out)
	}
	// A non-administrator signs in but has no admin API.
	devC, err := login("dev", "dev password 1")
	if err != nil {
		t.Fatal(err)
	}
	if err := devC.Do(ctx, http.MethodGet, "/api/me", nil, &me); err != nil || me.Admin {
		t.Fatalf("dev me: %+v %v", me, err)
	}
	if err := devC.Do(ctx, http.MethodGet, "/api/targets", nil, nil); statusOf(err) != 403 {
		t.Fatalf("dev used the admin API: %v", err)
	}

	// Changing one's own password needs the current one and ends the
	// other sessions; the old password stops working for git too.
	if err := devC.Do(ctx, http.MethodPost, "/api/me/password", map[string]string{"current": "nope nope nope", "password": "dev password 2"}, nil); statusOf(err) != 403 {
		t.Fatalf("change with a wrong current password: %v", err)
	}
	other, _ := login("dev", "dev password 1")
	var renewed sessionRes
	if err := devC.Do(ctx, http.MethodPost, "/api/me/password", map[string]string{"current": "dev password 1", "password": "dev password 2"}, &renewed); err != nil {
		t.Fatal(err)
	}
	if err := other.Do(ctx, http.MethodGet, "/api/me", nil, nil); err == nil {
		t.Fatal("another session survived the password change")
	}
	if err := client.New(e.cfg.PublicURL, renewed.Token).Do(ctx, http.MethodGet, "/api/me", nil, nil); err != nil {
		t.Fatalf("the renewed session: %v", err)
	}
	if _, err := gitCmd("", "ls-remote", gitURL("dev", "dev%20password%201", "acme/app")); err == nil {
		t.Fatal("the old password still works for git")
	}

	// A forgotten password is reset with the bootstrap admin token.
	if err := anon.Do(ctx, http.MethodPost, "/api/auth/reset", map[string]string{"admin_token": "wrong", "username": "root", "password": "new root pw"}, nil); statusOf(err) != 401 {
		t.Fatalf("reset with a wrong token: %v", err)
	}
	if err := anon.Do(ctx, http.MethodPost, "/api/auth/reset", map[string]string{"admin_token": adminToken, "username": "root", "password": "new root pw"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := root.Do(ctx, http.MethodGet, "/api/me", nil, nil); err == nil {
		t.Fatal("a session survived the reset")
	}
	root, err = login("root", "new root pw")
	if err != nil {
		t.Fatal(err)
	}

	// The last administrator can be neither removed nor demoted nor disabled.
	for _, req := range []struct {
		method, path string
		body         any
	}{
		{http.MethodDelete, "/api/users/root", nil},
		{http.MethodPatch, "/api/users/root", map[string]any{"admin": false}},
		{http.MethodPatch, "/api/users/root", map[string]any{"disabled": true}},
	} {
		if err := root.Do(ctx, req.method, req.path, req.body, nil); statusOf(err) != 409 {
			t.Fatalf("%s %s: %v", req.method, req.path, err)
		}
	}
	// A disabled account can neither sign in nor use git.
	if err := root.Do(ctx, http.MethodPatch, "/api/users/dev", map[string]any{"disabled": true}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := login("dev", "dev password 2"); err == nil {
		t.Fatal("a disabled account signed in")
	}
	if _, err := gitCmd("", "ls-remote", gitURL("dev", "dev%20password%202", "acme/app")); err == nil {
		t.Fatal("a disabled account used git")
	}

	// Guessing is slowed down: after a few failures the account is locked
	// for a while, even for the right password.
	for i := 0; i < 5; i++ {
		login("root", "guess guess")
	}
	if _, err := login("root", "new root pw"); statusOf(err) != 429 {
		t.Fatalf("no lockout: %v", err)
	}

	// Sign-ins and account changes are audited under the account's name.
	var audit struct {
		Entries []struct {
			Actor  string `json:"actor"`
			Action string `json:"action"`
		} `json:"entries"`
	}
	e.do(http.MethodGet, "/api/audit?limit=200", nil, &audit)
	seen := map[string]bool{}
	for _, a := range audit.Entries {
		seen[a.Actor+" "+a.Action] = true
	}
	for _, want := range []string{"user:root create first account", "user:root sign in", "user:root reset password", "user:root git push", "user:root POST /api/users"} {
		if !seen[want] {
			t.Errorf("audit lacks %q: %v", want, seen)
		}
	}
}
