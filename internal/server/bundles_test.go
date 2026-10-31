package server_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwq-inc/gitrepo/internal/client"
	"github.com/qwq-inc/gitrepo/internal/config"
)

func TestBundleURI(t *testing.T) {
	e := newEnv(t, func(c *config.Config) {
		c.Bundles.Enabled = true
		c.Bundles.MinSize = 0
	})
	var dev client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	for i := 0; i < 5; i++ {
		e.commit(work, "f.txt", strings.Repeat("x", i+1)+"\n")
	}
	hub := e.hubURL(dev.Token, "acme/big")
	e.run(work, "push", "-q", hub, "main")
	hubDir := e.hubRepo("acme/big")
	e.git.UpdateRefs(t.Context(), hubDir, []string{"update refs/backup/20260101T000000.000Z/refs/heads/old " + strings.TrimSpace(e.run(work, "rev-parse", "HEAD~1"))})
	e.srv.RefreshBundles(t.Context(), true)

	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(e.reposDir), "bundles", "acme", "big", "*.bundle"))
	if len(matches) != 1 {
		t.Fatalf("bundles: %v", matches)
	}
	heads := e.run("", "bundle", "list-heads", matches[0])
	if strings.Contains(heads, "refs/backup") || !strings.Contains(heads, "refs/heads/main") {
		t.Fatalf("bundle heads:\n%s", heads)
	}

	t.Run("an opted-in clone downloads the bundle, then fetches the rest", func(t *testing.T) {
		e.commit(work, "after.txt", "after the bundle\n")
		e.run(work, "push", "-q", hub, "main")
		clone := filepath.Join(t.TempDir(), "clone")
		e.run("", "-c", "protocol.version=2", "-c", "transfer.bundleURI=true", "clone", "-q", hub, clone)
		if got := strings.TrimSpace(e.run(clone, "rev-parse", "HEAD")); got != strings.TrimSpace(e.run(work, "rev-parse", "HEAD")) {
			t.Fatal("clone is incomplete")
		}
		if !strings.Contains(e.logs.String(), "path=/acme/big.git/bundles/") || !strings.Contains(e.logs.String(), ".bundle status=200") {
			t.Fatal("the bundle was not downloaded")
		}
	})

	t.Run("private bundle links need the advertised token", func(t *testing.T) {
		url := "http://" + e.hubAddr + "/acme/big.git/bundles/" + strings.TrimSuffix(filepath.Base(matches[0]), ".bundle") + ".bundle"
		for _, u := range []string{url, url + "?t=lfs1.forged.sig"} {
			resp, err := http.Get(u)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s: %d", u, resp.StatusCode)
			}
		}
	})

	t.Run("a re-created repository never gets the old bundle", func(t *testing.T) {
		e.do(http.MethodDelete, "/api/repos/acme/big", nil, nil)
		if _, err := os.Stat(filepath.Join(filepath.Dir(e.reposDir), "bundles", "acme", "big")); !os.IsNotExist(err) {
			t.Fatal("bundles of a deleted repository were kept")
		}
		e.do(http.MethodPost, "/api/repos", map[string]any{"slug": "acme/big", "bindings": false}, nil)
		other := filepath.Join(t.TempDir(), "other")
		e.run("", "init", "-q", "-b", "main", other)
		e.commit(other, "new.txt", "new project\n")
		e.run(other, "push", "-q", hub, "main")
		before := strings.Count(e.logs.String(), ".bundle status=")
		clone := filepath.Join(t.TempDir(), "clone2")
		e.run("", "-c", "protocol.version=2", "-c", "transfer.bundleURI=true", "clone", "-q", hub, clone)
		if after := strings.Count(e.logs.String(), ".bundle status="); after != before {
			t.Fatal("a bundle was offered for the re-created repository")
		}
	})
}
