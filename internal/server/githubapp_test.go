package server_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwq-inc/gitrepo/internal/client"
)

func TestGitHubAppCredential(t *testing.T) {
	e := newEnv(t)
	fake := newFakePlatforms(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	fake.installApp("4242", &key.PublicKey, map[string]int{"acme": 77})
	ghTarget := map[string]any{
		"name": "github", "provider": "github", "namespace": "acme", "base_url": fake.srv.URL + "/gh",
		"credential": map[string]any{"kind": "github-app", "app_id": "4242", "secret": keyPEM},
	}

	t.Run("a wrong key or App ID is refused when the target is saved", func(t *testing.T) {
		other, _ := rsa.GenerateKey(rand.Reader, 2048)
		bad := map[string]any{"name": "bad", "provider": "github", "namespace": "acme", "base_url": fake.srv.URL + "/gh",
			"credential": map[string]any{"kind": "github-app", "app_id": "4242",
				"secret": string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(other)}))}}
		if err := e.api.Do(t.Context(), http.MethodPost, "/api/targets", bad, nil); err == nil {
			t.Fatal("a key the App does not have was accepted")
		}
		bad["credential"] = map[string]any{"kind": "github-app", "app_id": "4242", "secret": "not a key"}
		if err := e.api.Do(t.Context(), http.MethodPost, "/api/targets", bad, nil); err == nil || !strings.Contains(err.Error(), "PEM") {
			t.Fatalf("garbage key: %v", err)
		}
		bad["provider"], bad["base_url"] = "gitlab", fake.srv.URL+"/gl"
		if err := e.api.Do(t.Context(), http.MethodPost, "/api/targets", bad, nil); err == nil {
			t.Fatal("github-app accepted for a gitlab target")
		}
	})

	var out client.Target
	var raw map[string]any
	e.do(http.MethodPost, "/api/targets", ghTarget, &raw)
	b, _ := json.Marshal(raw)
	json.Unmarshal(b, &out)

	t.Run("the installation is found for the namespace and the key never comes back", func(t *testing.T) {
		if raw["credential_kind"] != "github-app" || raw["app_id"] != "4242" || raw["installation_id"] != "77" {
			t.Fatalf("target: %v", raw)
		}
		if strings.Contains(string(b), "PRIVATE KEY") {
			t.Fatal("the private key was returned")
		}
		var res struct {
			OK       bool   `json:"ok"`
			Identity string `json:"identity"`
		}
		e.do(http.MethodPost, "/api/targets/github/check", nil, &res)
		if !res.OK || !strings.Contains(res.Identity, "installation 77") {
			t.Fatalf("check: %+v", res)
		}
	})

	t.Run("pushes create and fill mirrors with installation tokens, minted once and reused", func(t *testing.T) {
		e.do(http.MethodPost, "/api/bindings", map[string]any{"org": "*", "target": "github"}, nil)
		var dev client.Token
		e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
		hub := e.hubURL(dev.Token, "acme/appdemo")
		work := filepath.Join(t.TempDir(), "work")
		e.run("", "init", "-q", "-b", "main", work)
		e.commit(work, "a.txt", "1\n")
		mustContain(t, e.run(work, "push", hub, "main"), "[ok]       github:acme/appdemo")
		if !fake.called("gh create org acme/appdemo private=true") {
			t.Fatalf("repository not created through the App: %v", fake.calls)
		}
		e.commit(work, "a.txt", "2\n")
		mustContain(t, e.run(work, "push", hub, "main"), "[ok]       github:acme/appdemo")
		sameRefs(t, "github", e.refs(filepath.Join(fake.dir, "gh", "acme", "appdemo.git")), e.refs(e.hubRepo("acme/appdemo")))
		if n := fake.appMints(); n != 1 {
			t.Fatalf("installation tokens minted: %d, want 1 (cached)", n)
		}
	})

	t.Run("audit entries never contain the key", func(t *testing.T) {
		var page auditPage
		e.do(http.MethodGet, "/api/audit", nil, &page)
		raw, _ := json.Marshal(page)
		if strings.Contains(string(raw), "PRIVATE KEY") || strings.Contains(string(raw), "ghs_fake") {
			t.Fatalf("secret in the audit log: %s", raw)
		}
		if page.find(func(r auditRow) bool { return r.Detail == "target github (github, credential github-app)" }) == nil {
			t.Fatalf("audit: %+v", page.Entries)
		}
	})
}
