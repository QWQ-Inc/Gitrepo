package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/client"
	"github.com/qwq-inc/gitrepo/internal/config"
)

type auditRow struct {
	ID         int64     `json:"id"`
	At         time.Time `json:"at"`
	Actor      string    `json:"actor"`
	Via        string    `json:"via"`
	ClientAddr string    `json:"client_addr"`
	Action     string    `json:"action"`
	Path       string    `json:"path"`
	Repo       string    `json:"repo"`
	Status     int       `json:"status"`
	Detail     string    `json:"detail"`
}

type auditPage struct {
	Entries []auditRow `json:"entries"`
	Next    int64      `json:"next"`
}

func (p auditPage) find(match func(auditRow) bool) *auditRow {
	for i := range p.Entries {
		if match(p.Entries[i]) {
			return &p.Entries[i]
		}
	}
	return nil
}

func TestAuditLog(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.TrustedProxies = []string{"127.0.0.1"} })
	base := "http://" + e.hubAddr
	const credential = "glpat-very-secret-value"

	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "mirror-a", "provider": "generic", "url_template": e.remoteURL + "/a/{name}.git",
		"credential": map[string]any{"kind": "token", "secret": credential},
	}, nil)
	e.do(http.MethodPatch, "/api/targets/mirror-a", map[string]any{
		"credential": map[string]any{"kind": "basic", "username": userA, "secret": passA},
	}, nil)
	var dev client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}, "repos": []string{"acme/*"}}, &dev)
	if err := e.api.Do(t.Context(), http.MethodPost, "/api/tokens", map[string]any{"name": "admin", "scopes": []string{"read"}}, nil); err == nil {
		t.Fatal("a token named admin was accepted; it would be indistinguishable from the bootstrap token")
	}

	// Pushes: create, fast-forward, forced.
	hub := e.hubURL(dev.Token, "acme/app")
	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	first := e.commit(work, "a.txt", "1\n")
	e.run(work, "push", "-q", hub, "main")
	second := e.commit(work, "a.txt", "2\n")
	e.run(work, "push", "-q", hub, "main")
	e.run(work, "reset", "-q", "--hard", "HEAD~1")
	e.run(work, "push", "-q", "--force", hub, "main")

	// A token without admin scope, a bad token, a read, and a call through
	// the trusted reverse proxy.
	if err := client.New(base, dev.Token).Do(t.Context(), http.MethodDelete, "/api/targets/mirror-a", nil, nil); err == nil {
		t.Fatal("non-admin token deleted a target")
	}
	if err := client.New(base, "wrong").Do(t.Context(), http.MethodDelete, "/api/targets/mirror-a", nil, nil); err == nil {
		t.Fatal("bad token deleted a target")
	}
	e.do(http.MethodGet, "/api/targets", nil, nil)
	body, _ := json.Marshal(map[string]any{"org": "acme", "target": "mirror-a"})
	req, _ := http.NewRequest(http.MethodPost, base+"/api/bindings", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("X-Forwarded-For", "6.6.6.6, 198.51.100.7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create binding: %s", resp.Status)
	}

	// A refused call whose path carries a newline and a terminal escape,
	// trying to forge an extra row in `gr audit`.
	forge, _ := http.NewRequest(http.MethodPost, base+"/api/repos/x%0A99%09admin/y%1b%5B2K/sync", nil)
	forge.Header.Set("Authorization", "Bearer "+dev.Token)
	if resp, err := http.DefaultClient.Do(forge); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	// An import that fails after the handler described it.
	if err := e.api.Do(t.Context(), http.MethodPost, "/api/repos/acme/ghost/import", map[string]any{"target": "mirror-a"}, nil); err == nil {
		t.Fatal("import of a missing repository succeeded")
	}

	var all auditPage
	e.do(http.MethodGet, "/api/audit", nil, &all)
	for i := 1; i < len(all.Entries); i++ {
		if all.Entries[i].ID >= all.Entries[i-1].ID {
			t.Fatal("entries are not newest first")
		}
	}

	t.Run("admin changes are recorded without secrets", func(t *testing.T) {
		create := all.find(func(r auditRow) bool { return r.Action == "POST /api/targets" })
		if create == nil || create.Actor != "admin" || create.Via != "api" || create.Status != 201 || create.Detail != "target mirror-a (generic, credential token)" || create.ClientAddr != "127.0.0.1" {
			t.Fatalf("target creation: %+v", create)
		}
		patch := all.find(func(r auditRow) bool { return r.Action == "PATCH /api/targets/{name}" })
		if patch == nil || patch.Path != "/api/targets/mirror-a" || patch.Detail != "changed credential" {
			t.Fatalf("target update: %+v", patch)
		}
		tok := all.find(func(r auditRow) bool { return r.Action == "POST /api/tokens" && r.Status == 201 })
		if tok == nil || tok.Detail != "token dev (scopes create; repos acme/*)" {
			t.Fatalf("token creation: %+v", tok)
		}
		if r := all.find(func(r auditRow) bool { return r.Action == "POST /api/tokens" && r.Status == 400 }); r == nil || r.Detail != `error: "admin" is reserved for the bootstrap admin token` {
			t.Fatalf("the rejected token request: %+v", r)
		}
		raw, _ := json.Marshal(all)
		for _, secret := range []string{credential, passA, dev.Token} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("audit log contains a secret: %s", raw)
			}
		}
	})

	t.Run("pushes are recorded with the pusher and forced updates", func(t *testing.T) {
		var pushes auditPage
		e.do(http.MethodGet, "/api/audit?action=push", nil, &pushes)
		if len(pushes.Entries) != 3 {
			t.Fatalf("pushes: %+v", pushes.Entries)
		}
		want := []string{
			fmt.Sprintf("main %s → %s (forced)", second[:12], first[:12]),
			fmt.Sprintf("main %s → %s", first[:12], second[:12]),
			fmt.Sprintf("main created at %s", first[:12]),
		}
		for i, p := range pushes.Entries {
			if p.Actor != "dev" || p.Via != "http" || p.Action != "git push" || p.Repo != "acme/app" || p.ClientAddr != "127.0.0.1" || p.Detail != want[i] {
				t.Errorf("push %d: %+v, want detail %q", i, p, want[i])
			}
		}
	})

	t.Run("refused calls are recorded, unauthenticated calls and reads are not", func(t *testing.T) {
		denied := all.find(func(r auditRow) bool { return r.Actor == "dev" && r.Via == "api" && r.Path == "/api/targets/mirror-a" })
		if denied == nil || denied.Status != 403 || denied.Action != "DELETE /api/targets/{name}" || denied.Detail != "error: admin scope required" {
			t.Fatalf("refused call: %+v", denied)
		}
		if r := all.find(func(r auditRow) bool { return r.Status == 401 || strings.HasPrefix(r.Action, "GET ") }); r != nil {
			t.Fatalf("unexpected entry: %+v", r)
		}
	})

	t.Run("client address comes from a trusted proxy's X-Forwarded-For", func(t *testing.T) {
		b := all.find(func(r auditRow) bool { return r.Action == "POST /api/bindings" })
		if b == nil || b.ClientAddr != "198.51.100.7" || b.Detail != "binding acme → mirror-a (name {name}, on divergence alert)" {
			t.Fatalf("binding: %+v", b)
		}
	})

	t.Run("control characters from the request are stored escaped", func(t *testing.T) {
		f := all.find(func(r auditRow) bool { return strings.Contains(r.Path, "/sync") })
		if f == nil || strings.ContainsAny(f.Path+f.Repo, "\n\t\x1b") || !strings.Contains(f.Path, `x\x0a99\x09admin/y\x1b[2K`) {
			t.Fatalf("forged path: %+v", f)
		}
	})

	t.Run("a failure keeps both the description and the reason", func(t *testing.T) {
		imp := all.find(func(r auditRow) bool { return r.Action == "POST /api/repos/{org}/{name}/import" })
		if imp == nil || imp.Status < 400 || !strings.HasPrefix(imp.Detail, "from mirror-a:ghost") || !strings.Contains(imp.Detail, "; error: ") {
			t.Fatalf("failed import: %+v", imp)
		}
	})

	t.Run("filters and pagination", func(t *testing.T) {
		var mine auditPage
		e.do(http.MethodGet, "/api/audit?actor=dev", nil, &mine)
		if len(mine.Entries) != 5 {
			t.Fatalf("actor filter: %+v", mine.Entries)
		}
		var repo auditPage
		e.do(http.MethodGet, "/api/audit?repo=acme/app", nil, &repo)
		if len(repo.Entries) != 3 {
			t.Fatalf("repo filter: %+v", repo.Entries)
		}
		var seen []int64
		path := "/api/audit?limit=3"
		for {
			var page auditPage
			e.do(http.MethodGet, path, nil, &page)
			for _, r := range page.Entries {
				seen = append(seen, r.ID)
			}
			if page.Next == 0 {
				break
			}
			path = fmt.Sprintf("/api/audit?limit=3&before=%d", page.Next)
		}
		if len(seen) != len(all.Entries) {
			t.Fatalf("paged %d entries, want %d", len(seen), len(all.Entries))
		}
		for i := range seen {
			if seen[i] != all.Entries[i].ID {
				t.Fatalf("page order differs at %d", i)
			}
		}
	})
}
