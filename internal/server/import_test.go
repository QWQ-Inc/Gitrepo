package server_test

import (
	"crypto/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/client"
	"github.com/qwq-inc/gitrepo/internal/lfs"
)

type importResponse struct {
	Import struct {
		Refs          int      `json:"refs"`
		LFSObjects    int      `json:"lfs_objects"`
		DefaultBranch string   `json:"default_branch"`
		BackedUp      int      `json:"backed_up"`
		Warnings      []string `json:"warnings"`
	} `json:"import"`
	Source string      `json:"source"`
	Repo   client.Repo `json:"repo"`
}

// An existing project on a platform (with branches, a tag and LFS content)
// becomes the hub's content and is then copied to the other mirrors.
func TestImportFromMirror(t *testing.T) {
	if _, err := gitCmd("", "lfs", "version"); err != nil {
		t.Skip("git-lfs is not installed")
	}
	e := newEnv(t)
	platformURL, platformDir := newLFSPlatform(t)

	// The existing project, pushed straight to the platform.
	existing := filepath.Join(platformDir, "lfs", "project.git")
	e.git.InitBare(t.Context(), existing, "trunk")
	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "trunk", work)
	e.run(work, "lfs", "install", "--local")
	e.run(work, "lfs", "track", "*.bin")
	blob := make([]byte, 64<<10)
	rand.Read(blob)
	os.WriteFile(filepath.Join(work, "model.bin"), blob, 0o644)
	e.run(work, "add", ".gitattributes", "model.bin")
	e.run(work, "commit", "-q", "-m", "model")
	e.run(work, "tag", "v1")
	e.run(work, "checkout", "-q", "-b", "dev")
	e.commit(work, "dev.txt", "dev\n")
	e.run(work, "checkout", "-q", "trunk")
	platform := strings.Replace(platformURL, "http://", "http://"+userA+":"+passA+"@", 1) + "/lfs/project.git"
	e.run(work, "push", "-q", platform, "trunk", "dev", "v1")
	oid := strings.Fields(e.run(work, "lfs", "ls-files", "--long"))[0]

	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "origin", "provider": "generic", "url_template": platformURL + "/lfs/{name}.git",
		"credential": map[string]any{"kind": "basic", "username": userA, "secret": passA},
	}, nil)
	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "backup", "provider": "generic", "url_template": platformURL + "/backup/{name}.git",
		"credential": map[string]any{"kind": "basic", "username": userA, "secret": passA},
	}, nil)
	e.do(http.MethodPost, "/api/bindings", map[string]any{"org": "acme", "target": "backup"}, nil)
	backup := filepath.Join(platformDir, "backup", "project.git")
	e.git.InitBare(t.Context(), backup, "main")
	hubDir := e.hubRepo("acme/project")

	t.Run("import copies refs, LFS objects and the default branch", func(t *testing.T) {
		var res importResponse
		e.do(http.MethodPost, "/api/repos/acme/project/import", map[string]any{"target": "origin"}, &res)
		if res.Import.Refs != 3 || res.Import.LFSObjects != 1 || res.Import.DefaultBranch != "trunk" || len(res.Import.Warnings) != 0 {
			t.Fatalf("import result: %+v", res.Import)
		}
		sameRefs(t, "hub", e.refs(hubDir), e.refs(existing))
		if !lfs.Has(hubDir, oid, int64(len(blob))) {
			t.Fatal("LFS object not imported")
		}
		if head, _ := e.git.SymbolicRef(t.Context(), hubDir); head != "refs/heads/trunk" {
			t.Fatalf("hub HEAD = %s", head)
		}
	})

	t.Run("the source is adopted and the other mirrors receive everything", func(t *testing.T) {
		deadline := time.Now().Add(20 * time.Second)
		for {
			st := e.status("acme/project")
			states := map[string]string{}
			for _, r := range st.Remotes {
				states[r.Target] = r.State
			}
			if states["origin"] == "synced" && states["backup"] == "synced" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("mirror states: %+v", st.Remotes)
			}
			time.Sleep(100 * time.Millisecond)
		}
		sameRefs(t, "backup", e.refs(backup), e.refs(hubDir))
		if !lfs.Has(backup, oid, int64(len(blob))) {
			t.Fatal("LFS object not synced to the other mirror")
		}
	})

	t.Run("a second import needs force and backs up the hub first", func(t *testing.T) {
		err := e.api.Do(t.Context(), http.MethodPost, "/api/repos/acme/project/import", map[string]any{"target": "origin"}, nil)
		if ce, ok := err.(*client.Error); !ok || ce.Status != http.StatusConflict {
			t.Fatalf("import into a non-empty hub: %v", err)
		}
		var res importResponse
		e.do(http.MethodPost, "/api/repos/acme/project/import", map[string]any{"target": "origin", "force": true}, &res)
		if res.Import.BackedUp != 3 {
			t.Fatalf("backed up %d refs", res.Import.BackedUp)
		}
	})

	t.Run("pushing to the hub afterwards updates every mirror", func(t *testing.T) {
		var dev client.Token
		e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"write"}}, &dev)
		tip := e.commit(work, "after.txt", "after import\n")
		out := e.run(work, "push", e.hubURL(dev.Token, "acme/project"), "trunk")
		mustContain(t, out, "[ok]       origin:project", "[ok]       backup:project")
		if e.refs(existing)["refs/heads/trunk"] != tip || e.refs(backup)["refs/heads/trunk"] != tip {
			t.Fatal("mirrors not updated after import")
		}
	})
}
