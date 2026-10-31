package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/qwq-inc/gitrepo/internal/gitserve"
)

// fakePlatforms emulates the subset of the GitHub and GitLab APIs Gitrepo
// uses, plus their Git smart HTTP endpoints, under /gh and /gl.
type fakePlatforms struct {
	t   *testing.T
	dir string
	srv *httptest.Server

	mu    sync.Mutex
	calls []string
}

const (
	ghToken = "gh-token"
	glToken = "gl-token"
	gtToken = "gt-token"
)

// glProtectHook makes the fake GitLab refuse non-fast-forward pushes to main
// until the protection rule allows force pushes (marker file).
const glProtectHook = `#!/bin/sh
while read old new ref; do
  if [ "$ref" = refs/heads/main ] && [ "$old" != 0000000000000000000000000000000000000000 ] && [ ! -f "$GIT_DIR/allow_force" ]; then
    if ! git merge-base --is-ancestor "$old" "$new"; then
      echo "GitLab: You are not allowed to force push code to a protected branch on this project."
      exit 1
    fi
  fi
done
`

func newFakePlatforms(t *testing.T) *fakePlatforms {
	f := &fakePlatforms{t: t, dir: t.TempDir()}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePlatforms) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakePlatforms) called(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == s {
			return true
		}
	}
	return false
}

func (f *fakePlatforms) initRepo(platform, path string) {
	dir := filepath.Join(f.dir, platform, path+".git")
	if out, err := gitCmd("", "init", "-q", "--bare", "-b", "main", dir); err != nil {
		f.t.Fatalf("init %s: %v %s", dir, err, out)
	}
	if platform == "gl" || platform == "gt" {
		if err := os.WriteFile(filepath.Join(dir, "hooks", "pre-receive"), []byte(glProtectHook), 0o755); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fakePlatforms) exists(platform, path string) bool {
	_, err := os.Stat(filepath.Join(f.dir, platform, path+".git", "HEAD"))
	return err == nil
}

func writeJSONResp(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (f *fakePlatforms) serve(w http.ResponseWriter, r *http.Request) {
	p := r.URL.EscapedPath()
	switch {
	case strings.HasPrefix(p, "/gh/api/v3/"):
		if r.Header.Get("Authorization") != "Bearer "+ghToken {
			writeJSONResp(w, 401, map[string]string{"message": "Bad credentials"})
			return
		}
		f.github(w, r, strings.TrimPrefix(p, "/gh/api/v3"))
	case strings.HasPrefix(p, "/gl/api/v4/"):
		if r.Header.Get("PRIVATE-TOKEN") != glToken {
			writeJSONResp(w, 401, map[string]string{"message": "401 Unauthorized"})
			return
		}
		f.gitlab(w, r, strings.TrimPrefix(p, "/gl/api/v4"))
	case strings.HasPrefix(p, "/gt/api/v1/"):
		if r.Header.Get("Authorization") != "token "+gtToken {
			writeJSONResp(w, 401, map[string]string{"message": "token is required"})
			return
		}
		f.gitea(w, r, strings.TrimPrefix(p, "/gt/api/v1"))
	default:
		platform, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		wantUser, wantPass := "x-access-token", ghToken
		switch platform {
		case "gl":
			wantUser, wantPass = "oauth2", glToken
		case "gt":
			wantUser, wantPass = "oauth2", gtToken
		}
		u, pw, ok := r.BasicAuth()
		if !ok || u != wantUser || pw != wantPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
			http.Error(w, "unauthorized", 401)
			return
		}
		root := filepath.Join(f.dir, platform)
		gitserve.RunBackend(w, r, "git", "/"+rest, []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=" + u, "HOME=" + root}, nil)
	}
}

func (f *fakePlatforms) github(w http.ResponseWriter, r *http.Request, p string) {
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	switch {
	case r.Method == "GET" && p == "/user":
		writeJSONResp(w, 200, map[string]string{"login": "octo"})
	case r.Method == "GET" && strings.HasPrefix(p, "/repos/"):
		if f.exists("gh", strings.TrimPrefix(p, "/repos/")) {
			writeJSONResp(w, 200, map[string]any{})
		} else {
			writeJSONResp(w, 404, map[string]string{"message": "Not Found"})
		}
	case r.Method == "POST" && strings.HasPrefix(p, "/orgs/") && strings.HasSuffix(p, "/repos"):
		org := strings.TrimSuffix(strings.TrimPrefix(p, "/orgs/"), "/repos")
		name, _ := body["name"].(string)
		f.record("gh create org " + org + "/" + name + " private=" + jsonStr(body["private"]))
		f.initRepo("gh", org+"/"+name)
		writeJSONResp(w, 201, map[string]any{})
	case r.Method == "PATCH" && strings.HasPrefix(p, "/repos/"):
		f.record("gh default_branch " + strings.TrimPrefix(p, "/repos/") + "=" + jsonStr(body["default_branch"]))
		writeJSONResp(w, 200, map[string]any{})
	default:
		writeJSONResp(w, 404, map[string]string{"message": "Not Found: " + r.Method + " " + p})
	}
}

func jsonStr(v any) string {
	b, _ := json.Marshal(v)
	return strings.Trim(string(b), `"`)
}

func (f *fakePlatforms) gitlab(w http.ResponseWriter, r *http.Request, p string) {
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	seg := strings.Split(strings.TrimPrefix(p, "/"), "/")
	unesc := func(s string) string { v, _ := url.PathUnescape(s); return v }
	switch {
	case r.Method == "GET" && p == "/user":
		writeJSONResp(w, 200, map[string]string{"username": "tanuki"})
	case r.Method == "GET" && len(seg) == 2 && seg[0] == "namespaces":
		if unesc(seg[1]) == "acme-group" {
			writeJSONResp(w, 200, map[string]any{"id": 7})
		} else {
			writeJSONResp(w, 404, map[string]string{"message": "404 Namespace Not Found"})
		}
	case r.Method == "POST" && p == "/projects":
		name, _ := body["path"].(string)
		f.record("gl create ns=" + jsonStr(body["namespace_id"]) + " " + name + " visibility=" + jsonStr(body["visibility"]))
		f.initRepo("gl", "acme-group/"+name)
		writeJSONResp(w, 201, map[string]any{})
	case len(seg) >= 2 && seg[0] == "projects":
		project := unesc(seg[1])
		if !f.exists("gl", project) {
			writeJSONResp(w, 404, map[string]string{"message": "404 Project Not Found"})
			return
		}
		repoDir := filepath.Join(f.dir, "gl", project+".git")
		switch {
		case r.Method == "GET" && len(seg) == 2:
			writeJSONResp(w, 200, map[string]any{})
		case r.Method == "PUT" && len(seg) == 2:
			f.record("gl default_branch " + project + "=" + jsonStr(body["default_branch"]))
			writeJSONResp(w, 200, map[string]any{})
		case r.Method == "GET" && len(seg) == 3 && seg[2] == "protected_branches":
			_, err := os.Stat(filepath.Join(repoDir, "allow_force"))
			writeJSONResp(w, 200, []map[string]any{{"name": "main", "allow_force_push": err == nil}})
		case r.Method == "PATCH" && len(seg) == 4 && seg[2] == "protected_branches":
			f.record("gl allow_force_push " + project + " " + unesc(seg[3]) + "=" + jsonStr(body["allow_force_push"]))
			os.WriteFile(filepath.Join(repoDir, "allow_force"), nil, 0o644)
			writeJSONResp(w, 200, map[string]any{})
		default:
			writeJSONResp(w, 404, map[string]string{"message": "404 Not Found"})
		}
	default:
		writeJSONResp(w, 404, map[string]string{"message": "404 Not Found"})
	}
}

func (f *fakePlatforms) gitea(w http.ResponseWriter, r *http.Request, p string) {
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	seg := strings.Split(strings.TrimPrefix(p, "/"), "/")
	switch {
	case r.Method == "GET" && p == "/user":
		writeJSONResp(w, 200, map[string]string{"login": "gitea-bot"})
	case r.Method == "POST" && len(seg) == 3 && seg[0] == "orgs" && seg[2] == "repos":
		name, _ := body["name"].(string)
		f.record("gt create org " + seg[1] + "/" + name + " private=" + jsonStr(body["private"]))
		f.initRepo("gt", seg[1]+"/"+name)
		writeJSONResp(w, 201, map[string]any{})
	case len(seg) >= 3 && seg[0] == "repos":
		project := seg[1] + "/" + seg[2]
		if !f.exists("gt", project) {
			writeJSONResp(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		repoDir := filepath.Join(f.dir, "gt", project+".git")
		switch {
		case r.Method == "GET" && len(seg) == 3:
			writeJSONResp(w, 200, map[string]any{})
		case r.Method == "PATCH" && len(seg) == 3:
			f.record("gt default_branch " + project + "=" + jsonStr(body["default_branch"]))
			writeJSONResp(w, 200, map[string]any{})
		case r.Method == "GET" && len(seg) == 4 && seg[3] == "branch_protections":
			_, err := os.Stat(filepath.Join(repoDir, "allow_force"))
			writeJSONResp(w, 200, []map[string]any{{"rule_name": "main", "enable_force_push": err == nil}})
		case r.Method == "PATCH" && len(seg) == 5 && seg[3] == "branch_protections":
			f.record("gt enable_force_push " + project + " " + seg[4] + "=" + jsonStr(body["enable_force_push"]))
			os.WriteFile(filepath.Join(repoDir, "allow_force"), nil, 0o644)
			writeJSONResp(w, 200, map[string]any{})
		default:
			writeJSONResp(w, 404, map[string]string{"message": "Not Found"})
		}
	default:
		writeJSONResp(w, 404, map[string]string{"message": "Not Found"})
	}
}

func TestPlatformAdapters(t *testing.T) {
	e := newEnv(t)
	fake := newFakePlatforms(t)
	const slug = "acme/svc"

	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "github", "provider": "github", "namespace": "acme", "base_url": fake.srv.URL + "/gh",
		"credential": map[string]any{"kind": "token", "secret": ghToken},
	}, nil)
	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "gitlab", "provider": "gitlab", "namespace": "acme-group", "base_url": fake.srv.URL + "/gl",
		"credential": map[string]any{"kind": "token", "secret": glToken},
	}, nil)

	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "forgejo", "provider": "gitea", "namespace": "acme", "base_url": fake.srv.URL + "/gt",
		"credential": map[string]any{"kind": "token", "secret": gtToken},
	}, nil)

	t.Run("credential check reports the platform identity", func(t *testing.T) {
		var res struct {
			OK       bool   `json:"ok"`
			Identity string `json:"identity"`
		}
		e.do(http.MethodPost, "/api/targets/github/check", nil, &res)
		if !res.OK || res.Identity != "octo" {
			t.Fatalf("github check: %+v", res)
		}
		e.do(http.MethodPost, "/api/targets/gitlab/check", nil, &res)
		if !res.OK || res.Identity != "tanuki" {
			t.Fatalf("gitlab check: %+v", res)
		}
		e.do(http.MethodPost, "/api/targets/forgejo/check", nil, &res)
		if !res.OK || res.Identity != "gitea-bot" {
			t.Fatalf("gitea check: %+v", res)
		}
	})

	e.do(http.MethodPost, "/api/bindings", map[string]any{"org": "*", "target": "github"}, nil)
	e.do(http.MethodPost, "/api/bindings", map[string]any{"org": "*", "target": "gitlab"}, nil)
	e.do(http.MethodPost, "/api/bindings", map[string]any{"org": "*", "target": "forgejo"}, nil)
	var dev struct {
		Token string `json:"token"`
	}
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)

	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	e.commit(work, "main.go", "package main\n")
	hub := e.hubURL(dev.Token, slug)
	ghRepo := filepath.Join(fake.dir, "gh", "acme", "svc.git")
	glRepo := filepath.Join(fake.dir, "gl", "acme-group", "svc.git")
	gtRepo := filepath.Join(fake.dir, "gt", "acme", "svc.git")

	t.Run("first push creates the repositories through the APIs", func(t *testing.T) {
		out := e.run(work, "push", hub, "main")
		mustContain(t, out, "[ok]       github:acme/svc", "[ok]       gitlab:acme-group/svc", "[ok]       forgejo:acme/svc")
		for _, c := range []string{
			"gh create org acme/svc private=true",
			"gl create ns=7 svc visibility=private",
			"gt create org acme/svc private=true",
			"gh default_branch acme/svc=main",
			"gl default_branch acme-group/svc=main",
			"gt default_branch acme/svc=main",
		} {
			if !fake.called(c) {
				t.Errorf("expected API call %q; got %v", c, fake.calls)
			}
		}
		want := e.refs(e.hubRepo(slug))
		sameRefs(t, "github", e.refs(ghRepo), want)
		sameRefs(t, "gitlab", e.refs(glRepo), want)
		sameRefs(t, "forgejo", e.refs(gtRepo), want)
	})

	t.Run("force push lifts GitLab and Gitea branch protection first", func(t *testing.T) {
		e.run(work, "commit", "-q", "--amend", "-m", "rewritten")
		out := e.run(work, "push", "--force", hub, "main")
		mustContain(t, out, "[ok]       github:acme/svc", "[ok]       gitlab:acme-group/svc", "[ok]       forgejo:acme/svc")
		for _, c := range []string{"gl allow_force_push acme-group/svc main=true", "gt enable_force_push acme/svc main=true"} {
			if !fake.called(c) {
				t.Fatalf("protection was not relaxed (%s); calls: %v", c, fake.calls)
			}
		}
		want := e.refs(e.hubRepo(slug))
		sameRefs(t, "gitlab", e.refs(glRepo), want)
		sameRefs(t, "forgejo", e.refs(gtRepo), want)
		sameRefs(t, "github", e.refs(ghRepo), want)
	})
}
