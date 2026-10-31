package server_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwq-inc/gitrepo/internal/client"
)

// Regression tests for mirror-safety issues found in review.
func TestMirrorSafety(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	target := func(name, dir string) {
		e.do(http.MethodPost, "/api/targets", map[string]any{
			"name": name, "provider": "generic", "url_template": e.remoteURL + "/" + dir + "/{name}.git",
			"credential": map[string]any{"kind": "token", "secret": tokenB},
		}, nil)
	}
	initRemote := func(path string) string {
		p := e.remoteRepo(path)
		if err := e.git.InitBare(ctx, p, "main"); err != nil {
			t.Fatal(err)
		}
		return p
	}
	var dev client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	e.commit(work, "a.txt", "a\n")
	e.run(work, "branch", "feature")

	t.Run("two hub repositories cannot share one mirror", func(t *testing.T) {
		target("shared", "e")
		e.do(http.MethodPost, "/api/bindings", map[string]any{"org": "team-a", "target": "shared"}, nil)
		e.do(http.MethodPost, "/api/bindings", map[string]any{"org": "team-b", "target": "shared"}, nil)
		e.do(http.MethodPost, "/api/bindings", map[string]any{"org": "team-c", "target": "shared", "name_template": "{org}-{name}"}, nil)
		initRemote("e/infra.git")
		initRemote("e/team-c-infra.git")

		mustContain(t, e.run(work, "push", e.hubURL(dev.Token, "team-a/infra"), "main"), "[ok]       shared:infra")
		out := e.run(work, "push", e.hubURL(dev.Token, "team-b/infra"), "main")
		mustContain(t, out, "binding team-b → shared was not applied", "shared:infra is already the mirror of team-a/infra", "no mirrors configured")
		mustContain(t, e.run(work, "push", e.hubURL(dev.Token, "team-c/infra"), "main"), "[ok]       shared:team-c-infra")

		err := e.api.Do(ctx, http.MethodPost, "/api/repos/team-b/infra/remotes", map[string]any{"target": "shared"}, nil)
		if ce, ok := err.(*client.Error); !ok || ce.Status != http.StatusConflict {
			t.Fatalf("adding a taken mirror location: %v", err)
		}
		e.do(http.MethodPost, "/api/repos/team-b/infra/remotes", map[string]any{"target": "shared", "name": "team-b-infra"}, nil)
	})

	t.Run("a non-empty pre-existing mirror is only written after adoption", func(t *testing.T) {
		target("existing", "f")
		e.do(http.MethodPost, "/api/bindings", map[string]any{"org": "legacy", "target": "existing"}, nil)
		legacy := initRemote("f/app.git")
		other := filepath.Join(t.TempDir(), "other")
		e.run("", "init", "-q", "-b", "main", other)
		foreign := e.commit(other, "theirs.txt", "someone else's project\n")
		e.run(other, "push", "-q", legacy, "main")

		out := e.run(work, "push", e.hubURL(dev.Token, "legacy/app"), "main", "feature")
		mustContain(t, out, "[failed]   existing:app", "adoption required", "rh remote adopt legacy/app")
		if refs := e.refs(legacy); len(refs) != 1 || refs["refs/heads/main"] != foreign {
			t.Fatalf("pre-existing mirror was modified before adoption: %v", refs)
		}
		rem := e.remoteByTarget("legacy/app", "existing")
		if !rem.NeedsAdoption {
			t.Fatalf("needs_adoption not reported: %+v", rem)
		}
		e.do(http.MethodPatch, "/api/repos/legacy/app/remotes/"+itoa(rem.ID), map[string]any{"adopt": true}, nil)
		res := e.sync("legacy/app", false)
		if ev := eventState(res, "existing"); ev.State != "diverged" || len(ev.Divergent) != 1 || ev.Divergent[0] != "refs/heads/main" {
			t.Fatalf("after adoption: %+v", res)
		}
		refs := e.refs(legacy)
		if refs["refs/heads/main"] != foreign || refs["refs/heads/feature"] == "" {
			t.Fatalf("after adoption: foreign main must stay, feature must arrive: %v", refs)
		}
	})

	t.Run("one rejected ref does not hold back the others", func(t *testing.T) {
		target("acl", "c")
		target("gated", "d")
		acl := initRemote("c/iso.git")
		gated := initRemote("d/iso.git")
		// gitolite-style per-ref ACL and GitLab-style all-or-nothing rule.
		os.WriteFile(filepath.Join(acl, "hooks", "update"), []byte("#!/bin/sh\n[ \"$1\" = refs/heads/forbidden ] && { echo denied; exit 1; }\nexit 0\n"), 0o755)
		os.WriteFile(filepath.Join(gated, "hooks", "pre-receive"), []byte("#!/bin/sh\nwhile read o n r; do [ \"$r\" = refs/heads/forbidden ] && { echo protected; exit 1; }; done\nexit 0\n"), 0o755)
		e.do(http.MethodPost, "/api/repos", map[string]any{"slug": "iso/iso", "bindings": false, "targets": []string{"acl", "gated"}}, nil)

		e.run(work, "branch", "forbidden")
		out := e.run(work, "push", e.hubURL(dev.Token, "iso/iso"), "main", "feature", "forbidden")
		mustContain(t, out, "[failed]   acl:iso", "[failed]   gated:iso", "refs/heads/forbidden")
		for name, repo := range map[string]string{"acl": acl, "gated": gated} {
			refs := e.refs(repo)
			if refs["refs/heads/main"] == "" || refs["refs/heads/feature"] == "" || refs["refs/heads/forbidden"] != "" {
				t.Fatalf("%s: innocent refs blocked or forbidden ref accepted: %v", name, refs)
			}
		}
	})

	t.Run("overwritten hub values are backed up even if the hook never ran", func(t *testing.T) {
		hubDir := e.hubRepo("team-a/infra")
		old := e.refs(hubDir)["refs/heads/main"]
		out, err := e.git.Run(ctx, hubDir, "commit-tree", "4b825dc642cb6eb9a060e54bf8d69288fbee4904", "-m", "rewritten behind the hook's back")
		if err != nil {
			t.Fatal(err)
		}
		rewritten := strings.TrimSpace(string(out))
		if err := e.git.UpdateRefs(ctx, hubDir, []string{"update refs/heads/main " + rewritten}); err != nil {
			t.Fatal(err)
		}
		if ev := eventState(e.sync("team-a/infra", false), "shared"); ev.State != "synced" {
			t.Fatalf("sync after rewrite: %+v", ev)
		}
		if e.refs(e.remoteRepo("e/infra.git"))["refs/heads/main"] != rewritten {
			t.Fatal("mirror not updated")
		}
		for ref, sha := range e.allRefs(hubDir) {
			if strings.HasPrefix(ref, "refs/backup/") && strings.HasSuffix(ref, "/refs/heads/main") && sha == old {
				return
			}
		}
		t.Fatalf("old value %s not preserved on the hub: %v", old, e.allRefs(hubDir))
	})

	t.Run("info/refs serves only the authorized service", func(t *testing.T) {
		e.do(http.MethodPost, "/api/repos", map[string]any{"slug": "pub/app", "visibility": "public", "bindings": false}, nil)
		resp, err := http.Get("http://" + e.hubAddr + "/pub/app.git/info/refs?service=git-upload-pack&service=git-receive-pack")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); resp.StatusCode != http.StatusOK || ct != "application/x-git-upload-pack-advertisement" {
			t.Fatalf("anonymous info/refs: %d %s", resp.StatusCode, ct)
		}
	})
}
