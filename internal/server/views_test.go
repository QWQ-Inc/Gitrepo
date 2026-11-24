package server_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/client"
)

// A monorepo on the hub, published as several repositories: one per
// directory (a split, with its own tags), one per branch, and a full mirror.
func TestMirrorViews(t *testing.T) {
	e := newEnv(t)
	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "mirror-a", "provider": "generic", "url_template": e.remoteURL + "/a/{name}.git",
		"credential": map[string]any{"kind": "basic", "username": userA, "secret": passA},
	}, nil)
	for _, p := range []string{"a/api.git", "a/web.git", "a/mono.git"} {
		if err := e.git.InitBare(t.Context(), e.remoteRepo(p), "main"); err != nil {
			t.Fatal(err)
		}
	}
	var dev client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	hub := e.hubURL(dev.Token, "acme/mono")

	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	e.commit(work, "README.md", "monorepo\n")
	writeAll(t, filepath.Join(work, "services/api/main.go"), []byte("package api\n"))
	e.run(work, "add", "services")
	e.run(work, "commit", "-q", "-m", "api: start")
	writeAll(t, filepath.Join(work, "services/web/index.html"), []byte("<h1>web</h1>\n"))
	e.run(work, "add", "services")
	e.run(work, "commit", "-q", "-m", "web: start")
	e.run(work, "tag", "-a", "api-v1.0", "-m", "API 1.0")
	e.run(work, "tag", "web-v2.0")
	e.run(work, "branch", "web")
	e.run(work, "push", "-q", hub, "main", "web", "api-v1.0", "web-v2.0")

	add := func(body map[string]any) {
		t.Helper()
		body["target"] = "mirror-a"
		e.do(http.MethodPost, "/api/repos/acme/mono/remotes", body, nil)
	}
	add(map[string]any{"name": "api", "subdir": "services/api", "refs": []string{"main", "refs/tags/api-v*:refs/tags/v*"}})
	add(map[string]any{"name": "web", "refs": []string{"web:main"}})
	add(map[string]any{"name": "mono"})
	if err := e.api.Do(t.Context(), http.MethodPost, "/api/repos/acme/mono/remotes", map[string]any{"target": "mirror-a", "name": "bad", "refs": []string{"main:refs/tags/main"}}, nil); err == nil {
		t.Fatal("an invalid view was accepted")
	}
	res := e.sync("acme/mono", false)
	if n := len(res.Events); n != 5 { // start, three mirrors, done
		t.Fatalf("sync events: %+v", res.Events)
	}
	for _, ev := range res.Events {
		if ev.Type == "remote" && ev.State != "synced" {
			t.Fatalf("%s: %s %s", ev.Remote, ev.State, ev.Error)
		}
	}
	hubRepo := e.hubRepo("acme/mono")
	rev := func(repo, spec string) string {
		t.Helper()
		return strings.TrimSpace(e.run("", "--git-dir="+repo, "rev-parse", spec))
	}

	// The directory split: only main and the renamed api tag; the root is
	// the directory; only the commit that touched it.
	api := e.refs(e.remoteRepo("a/api.git"))
	if len(api) != 2 || api["refs/heads/main"] == "" || api["refs/tags/v1.0"] == "" {
		t.Fatalf("api mirror refs: %v", api)
	}
	apiRepo := e.remoteRepo("a/api.git")
	if rev(apiRepo, "main^{tree}") != rev(hubRepo, "main:services/api") {
		t.Fatal("api mirror tree is not the directory")
	}
	if log := strings.TrimSpace(e.run("", "--git-dir="+apiRepo, "log", "--format=%s", "main")); log != "api: start" {
		t.Fatalf("api mirror history: %q", log)
	}
	if rev(apiRepo, "v1.0^{commit}") != api["refs/heads/main"] || strings.TrimSpace(e.run("", "--git-dir="+apiRepo, "cat-file", "-t", "v1.0")) != "tag" {
		t.Fatal("api tag")
	}
	// The branch mirror: the web branch as main, commits unchanged, no tags.
	web := e.refs(e.remoteRepo("a/web.git"))
	if len(web) != 1 || web["refs/heads/main"] != rev(hubRepo, "refs/heads/web") {
		t.Fatalf("web mirror refs: %v", web)
	}
	// The full mirror: everything.
	if mono := e.refs(e.remoteRepo("a/mono.git")); len(mono) != 4 {
		t.Fatalf("full mirror refs: %v", mono)
	}
	// The repository page describes the views.
	st := e.status("acme/mono")
	views := map[string]string{}
	for _, r := range st.Remotes {
		views[r.Name] = r.View
	}
	if !strings.Contains(views["api"], "directory services/api/") || !strings.Contains(views["web"], "web → main") || views["mono"] != "" {
		t.Fatalf("views: %v", views)
	}

	// A change outside the directory leaves the split mirror alone; a
	// change inside adds exactly one commit on top.
	before := api["refs/heads/main"]
	writeAll(t, filepath.Join(work, "services/web/index.html"), []byte("<h1>web 2</h1>\n"))
	e.run(work, "commit", "-q", "-am", "web: change")
	e.run(work, "push", "-q", hub, "main")
	e.sync("acme/mono", false)
	if e.refs(apiRepo)["refs/heads/main"] != before {
		t.Fatal("a web-only change moved the api mirror")
	}
	writeAll(t, filepath.Join(work, "services/api/main.go"), []byte("package api // v2\n"))
	e.run(work, "commit", "-q", "-am", "api: change")
	e.run(work, "push", "-q", hub, "main")
	eventually(t, "api mirror updated", 15*time.Second, func() bool { return e.refs(apiRepo)["refs/heads/main"] != before })
	if parent := rev(apiRepo, "main^"); parent != before {
		t.Fatalf("api mirror: new commit's parent %s, want %s", parent, before)
	}

	// Purging backups drops the splits; they come back identical.
	split := e.refs(apiRepo)["refs/heads/main"]
	e.do(http.MethodPost, "/api/repos/acme/mono/backups/purge", map[string]any{"all": true}, nil)
	if refs := e.allRefs(hubRepo); strings.Contains(strings.Join(keys(refs), " "), "refs/gitrepo/split/") {
		t.Fatalf("split refs survived the purge: %v", refs)
	}
	if ev := e.sync("acme/mono", false); len(ev.Events) == 0 {
		t.Fatal("no sync")
	}
	if e.refs(apiRepo)["refs/heads/main"] != split {
		t.Fatal("the split changed after a purge")
	}

	// Dropping the view turns the mirror into a full one at the next sync
	// (its own former values are backed up on the hub).
	rem := e.remoteByTarget("acme/mono", "mirror-a")
	for _, r := range st.Remotes {
		if r.Name == "api" {
			rem = r
		}
	}
	e.do(http.MethodPatch, "/api/repos/acme/mono/remotes/"+strconv.FormatInt(rem.ID, 10), map[string]any{"refs": []string{}, "subdir": ""}, nil)
	eventually(t, "api mirror is a full mirror", 15*time.Second, func() bool {
		return e.refs(apiRepo)["refs/heads/main"] == rev(hubRepo, "refs/heads/main")
	})
}

func writeAll(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, b)
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Releases follow the view: the tag's name on the mirror, and no release
// on a mirror that does not get the tag.
func TestReleasesFollowViews(t *testing.T) {
	e := newEnv(t)
	fake := newFakePlatforms(t)
	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "github", "provider": "github", "namespace": "acme", "base_url": fake.srv.URL + "/gh",
		"credential": map[string]any{"kind": "token", "secret": ghToken},
	}, nil)
	var dev client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	hub := e.hubURL(dev.Token, "acme/mono")
	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	writeAll(t, filepath.Join(work, "api/main.go"), []byte("package api\n"))
	writeAll(t, filepath.Join(work, "web/index.html"), []byte("web\n"))
	e.run(work, "add", ".")
	e.run(work, "commit", "-q", "-m", "start")
	e.run(work, "tag", "-a", "api-v1.0", "-m", "API 1.0")
	e.run(work, "tag", "-a", "web-v1.0", "-m", "Web 1.0")
	e.run(work, "push", "-q", hub, "main", "api-v1.0", "web-v1.0")
	e.do(http.MethodPost, "/api/repos/acme/mono/remotes", map[string]any{"target": "github", "name": "api", "subdir": "api", "refs": []string{"main", "refs/tags/api-v*:refs/tags/v*"}}, nil)
	e.do(http.MethodPost, "/api/repos/acme/mono/remotes", map[string]any{"target": "github", "name": "web", "subdir": "web", "refs": []string{"main", "refs/tags/web-v*:refs/tags/v*"}}, nil)
	if ev := e.sync("acme/mono", false); len(ev.Events) != 4 {
		t.Fatalf("sync: %+v", ev)
	}

	var out relOut
	e.do(http.MethodPost, "/api/repos/acme/mono/releases", map[string]any{"tag": "api-v1.0", "name": "API 1.0", "body": "first", "publish": true, "wait": true}, &out)
	if len(out.Mirrors) != 1 || out.Mirrors[0].Mirror != "github:acme/api" || out.Mirrors[0].State != "synced" {
		t.Fatalf("release mirrors: %+v", out.Mirrors)
	}
	if r := fake.release("gh", "acme/api", "v1.0"); r == nil || r.Name != "API 1.0" {
		t.Fatalf("release on the api mirror under its tag v1.0: %+v", r)
	}
	if fake.release("gh", "acme/web", "v1.0") != nil || fake.release("gh", "acme/api", "api-v1.0") != nil {
		t.Fatal("a release where the tag is not")
	}
}
