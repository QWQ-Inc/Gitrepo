package server_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/client"
	"github.com/qwq-inc/gitrepo/internal/config"
	"github.com/qwq-inc/gitrepo/internal/gitexec"
	"github.com/qwq-inc/gitrepo/internal/gitserve"
	"github.com/qwq-inc/gitrepo/internal/secrets"
	"github.com/qwq-inc/gitrepo/internal/server"
)

var gitrepoBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gitrepo-bin")
	if err != nil {
		panic(err)
	}
	gitrepoBin = filepath.Join(dir, "gitrepo")
	out, err := exec.Command("go", "build", "-o", gitrepoBin, "github.com/qwq-inc/gitrepo/cmd/gitrepo").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build gitrepo: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// ------------------------------------------------------------------ harness

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type env struct {
	t         *testing.T
	hubAddr   string
	reposDir  string
	remoteURL string
	remoteDir string
	api       *client.Client
	git       *gitexec.Git
}

const (
	adminToken = "admin-secret"
	userA      = "mirror"
	passA      = "s3cret"
	tokenB     = "tok-b"
)

func newEnv(t *testing.T, tweaks ...func(*config.Config)) *env {
	t.Helper()
	tmp := t.TempDir()
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server log:\n%s", logs.String())
		}
	})

	// A fake hosting platform: smart HTTP with basic auth per user.
	remoteDir := filepath.Join(tmp, "remote")
	users := map[string]string{userA: passA, "git": tokenB}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || users[u] != p {
			w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		gitserve.RunBackend(w, r, "git", r.URL.Path, []string{
			"GIT_PROJECT_ROOT=" + remoteDir, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=" + u, "HOME=" + remoteDir,
		}, log)
	}))
	t.Cleanup(remote.Close)

	g := &gitexec.Git{Bin: "git", Env: clientEnv()}
	for _, p := range []string{"a/demo.git", "b/demo.git"} {
		if err := g.InitBare(context.Background(), filepath.Join(remoteDir, p), "main"); err != nil {
			t.Fatal(err)
		}
	}

	cfg := config.Default()
	cfg.DataDir = filepath.Join(tmp, "data")
	cfg.Git.SelfPath = gitrepoBin
	cfg.SecretKeyEnv = "RH_TEST_SECRET_KEY"
	cfg.AdminTokenEnv = "RH_TEST_ADMIN_TOKEN"
	cfg.Sync.PostReceiveWait = config.Duration(30 * time.Second)
	cfg.Sync.PushTimeout = config.Duration(time.Minute)
	for _, f := range tweaks {
		f(cfg)
	}
	t.Setenv(cfg.SecretKeyEnv, secrets.GenerateKey())
	t.Setenv(cfg.AdminTokenEnv, adminToken)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.PublicURL = "http://" + ln.Addr().String()
	srv, err := server.New(cfg, log, server.Options{NoBackground: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(ln); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	return &env{
		t: t, hubAddr: ln.Addr().String(), reposDir: cfg.ReposDir(),
		remoteURL: remote.URL, remoteDir: remoteDir,
		api: client.New(cfg.PublicURL, adminToken), git: g,
	}
}

func clientEnv() []string {
	return []string{
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Dev", "GIT_AUTHOR_EMAIL=dev@example.com",
		"GIT_COMMITTER_NAME=Dev", "GIT_COMMITTER_EMAIL=dev@example.com",
	}
}

func (e *env) do(method, path string, in, out any) {
	e.t.Helper()
	if err := e.api.Do(context.Background(), method, path, in, out); err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
}

func (e *env) hubURL(token, slug string) string {
	if token == "" {
		return "http://" + e.hubAddr + "/" + slug + ".git"
	}
	return "http://x:" + token + "@" + e.hubAddr + "/" + slug + ".git"
}

// remoteAuthURL is an authenticated URL for poking at the fake platform
// directly (simulating someone bypassing the hub).
func (e *env) remoteAuthURL(path string) string {
	u := strings.TrimPrefix(e.remoteURL, "http://")
	return "http://git:" + tokenB + "@" + u + "/" + path
}

func gitCmd(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), clientEnv()...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *env) run(dir string, args ...string) string {
	e.t.Helper()
	out, err := gitCmd(dir, args...)
	if err != nil {
		e.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (e *env) refs(bareRepo string) map[string]string {
	e.t.Helper()
	r, err := e.git.HubRefs(context.Background(), bareRepo)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) allRefs(bareRepo string) map[string]string {
	e.t.Helper()
	r, err := e.git.ForEachRef(context.Background(), bareRepo)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) hubRepo(slug string) string { return filepath.Join(e.reposDir, slug+".git") }
func (e *env) remoteRepo(p string) string { return filepath.Join(e.remoteDir, p) }

func (e *env) commit(dir, file, content string) string {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
	e.run(dir, "add", file)
	e.run(dir, "commit", "-q", "-m", "change "+file)
	return strings.TrimSpace(e.run(dir, "rev-parse", "HEAD"))
}

func (e *env) status(slug string) client.Repo {
	e.t.Helper()
	var r client.Repo
	e.do(http.MethodGet, "/api/repos/"+slug, nil, &r)
	return r
}

func (e *env) remoteByTarget(slug, target string) client.Remote {
	e.t.Helper()
	for _, r := range e.status(slug).Remotes {
		if r.Target == target {
			return r
		}
	}
	e.t.Fatalf("no remote for target %s", target)
	return client.Remote{}
}

func (e *env) sync(slug string, overwrite bool) client.SyncResult {
	e.t.Helper()
	var res client.SyncResult
	e.do(http.MethodPost, "/api/repos/"+slug+"/sync", map[string]any{"wait": true, "overwrite": overwrite}, &res)
	return res
}

func eventState(res client.SyncResult, target string) client.Event {
	for _, ev := range res.Events {
		if ev.Type == "remote" && strings.HasPrefix(ev.Remote, target+":") {
			return ev
		}
	}
	return client.Event{}
}

func mustContain(t *testing.T, out string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(out, s) {
			t.Fatalf("output does not contain %q:\n%s", s, out)
		}
	}
}

func sameRefs(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d refs %v, want %d refs %v", what, len(got), got, len(want), want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: %s = %q, want %q", what, k, got[k], v)
		}
	}
}

// --------------------------------------------------------------------- test

func TestEndToEnd(t *testing.T) {
	e := newEnv(t)
	const slug = "acme/demo"

	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "mirror-a", "provider": "generic", "url_template": e.remoteURL + "/a/{name}.git",
		"credential": map[string]any{"kind": "basic", "username": userA, "secret": passA},
	}, nil)
	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "mirror-b", "provider": "generic", "url_template": e.remoteURL + "/b/{name}.git",
		"credential": map[string]any{"kind": "token", "secret": tokenB},
	}, nil)
	e.do(http.MethodPost, "/api/bindings", map[string]any{"org": "*", "target": "mirror-a"}, nil)
	e.do(http.MethodPost, "/api/bindings", map[string]any{"org": "*", "target": "mirror-b"}, nil)
	var dev, ci client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "ci", "scopes": []string{"read"}, "repos": []string{"acme/*"}}, &ci)

	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	e.commit(work, "README.md", "hello\n")
	e.run(work, "tag", "-a", "v1", "-m", "v1")
	e.run(work, "checkout", "-q", "-b", "feature")
	e.commit(work, "feature.txt", "feature\n")
	e.run(work, "checkout", "-q", "main")

	hub := e.hubURL(dev.Token, slug)
	hubDir := e.hubRepo(slug)
	remA, remB := e.remoteRepo("a/demo.git"), e.remoteRepo("b/demo.git")

	t.Run("anonymous push is refused", func(t *testing.T) {
		out, err := gitCmd(work, "push", e.hubURL("", slug), "main")
		if err == nil {
			t.Fatalf("push succeeded:\n%s", out)
		}
		var r client.Repo
		if err := e.api.Do(context.Background(), http.MethodGet, "/api/repos/"+slug, nil, &r); err == nil {
			t.Fatal("repository was created by an anonymous push")
		}
	})

	t.Run("first push creates the repository and mirrors it", func(t *testing.T) {
		out := e.run(work, "push", hub, "main", "feature", "refs/tags/v1")
		mustContain(t, out, "syncing to 2 mirror(s)", "[ok]       mirror-a:demo", "[ok]       mirror-b:demo")
		want := e.refs(hubDir)
		if len(want) != 3 {
			t.Fatalf("hub refs: %v", want)
		}
		sameRefs(t, "mirror-a", e.refs(remA), want)
		sameRefs(t, "mirror-b", e.refs(remB), want)
		st := e.status(slug)
		if st.State != "synced" || st.DefaultBranch != "main" {
			t.Fatalf("status: %+v", st)
		}
	})

	t.Run("read token can clone but not push", func(t *testing.T) {
		clone := filepath.Join(t.TempDir(), "clone")
		e.run("", "clone", "-q", e.hubURL(ci.Token, slug), clone)
		out, err := gitCmd(clone, "push", e.hubURL(ci.Token, slug), "HEAD:refs/heads/ci")
		if err == nil {
			t.Fatalf("read-only push succeeded:\n%s", out)
		}
		mustContain(t, out, "403")
	})

	t.Run("force push is mirrored and the old tip is kept on the hub", func(t *testing.T) {
		old := strings.TrimSpace(e.run(work, "rev-parse", "main"))
		e.run(work, "commit", "-q", "--amend", "-m", "rewritten")
		newTip := strings.TrimSpace(e.run(work, "rev-parse", "main"))
		out := e.run(work, "push", "--force", hub, "main")
		mustContain(t, out, "kept 1 overwritten/deleted ref(s) under refs/backup/", "[ok]       mirror-a:demo  1 ref(s) updated")
		if e.refs(remA)["refs/heads/main"] != newTip || e.refs(remB)["refs/heads/main"] != newTip {
			t.Fatal("mirrors did not receive the forced update")
		}
		found := false
		for ref, sha := range e.allRefs(hubDir) {
			if strings.HasPrefix(ref, "refs/backup/") && strings.HasSuffix(ref, "/refs/heads/main") && sha == old {
				found = true
			}
		}
		if !found {
			t.Fatalf("no backup of %s on the hub: %v", old, e.allRefs(hubDir))
		}
		ls := e.run(work, "ls-remote", hub)
		if strings.Contains(ls, "refs/backup") {
			t.Fatalf("backup refs are advertised to clients:\n%s", ls)
		}
	})

	t.Run("divergent mirror is left alone while other refs still sync", func(t *testing.T) {
		rogue := filepath.Join(t.TempDir(), "rogue")
		e.run("", "clone", "-q", e.remoteAuthURL("b/demo.git"), rogue)
		foreign := e.commit(rogue, "rogue.txt", "pushed straight to mirror-b\n")
		e.run(rogue, "push", "-q", "origin", "main")

		mainTip := e.commit(work, "main.txt", "main 2\n")
		e.run(work, "checkout", "-q", "feature")
		featTip := e.commit(work, "feature.txt", "feature 2\n")
		e.run(work, "checkout", "-q", "main")
		out := e.run(work, "push", hub, "main", "feature")
		mustContain(t, out, "[ok]       mirror-a:demo", "[diverged] mirror-b:demo  refs/heads/main", "gr sync acme/demo --overwrite")

		if e.refs(remA)["refs/heads/main"] != mainTip {
			t.Fatal("mirror-a main not updated")
		}
		b := e.refs(remB)
		if b["refs/heads/main"] != foreign {
			t.Fatal("divergent main on mirror-b was overwritten")
		}
		if b["refs/heads/feature"] != featTip {
			t.Fatal("feature on mirror-b was blocked by the divergent main")
		}
		rb := e.remoteByTarget(slug, "mirror-b")
		if rb.State != "diverged" || len(rb.Divergences) != 1 || rb.Divergences[0].Ref != "refs/heads/main" || rb.Divergences[0].RemoteSHA != foreign {
			t.Fatalf("mirror-b status: %+v", rb)
		}
		if e.status(slug).State != "diverged" {
			t.Fatal("repo state should be diverged")
		}
	})

	t.Run("merging the mirror's commits resolves the divergence", func(t *testing.T) {
		e.run(work, "fetch", "-q", e.remoteAuthURL("b/demo.git"), "main")
		e.run(work, "merge", "-q", "--no-edit", "FETCH_HEAD")
		tip := strings.TrimSpace(e.run(work, "rev-parse", "HEAD"))
		out := e.run(work, "push", hub, "main")
		mustContain(t, out, "[ok]       mirror-b:demo")
		if e.refs(remB)["refs/heads/main"] != tip {
			t.Fatal("mirror-b main not fast-forwarded")
		}
		rb := e.remoteByTarget(slug, "mirror-b")
		if rb.State != "synced" || len(rb.Divergences) != 0 {
			t.Fatalf("mirror-b status: %+v", rb)
		}
	})

	t.Run("deleting a branch on the hub deletes it on the mirrors", func(t *testing.T) {
		out := e.run(work, "push", hub, ":feature")
		mustContain(t, out, "[ok]       mirror-a:demo  1 ref(s) updated")
		if _, ok := e.refs(remA)["refs/heads/feature"]; ok {
			t.Fatal("feature still on mirror-a")
		}
		if _, ok := e.refs(remB)["refs/heads/feature"]; ok {
			t.Fatal("feature still on mirror-b")
		}
	})

	t.Run("overwrite replaces divergent refs after backing them up", func(t *testing.T) {
		rogue := filepath.Join(t.TempDir(), "rogue")
		e.run("", "clone", "-q", e.remoteAuthURL("b/demo.git"), rogue)
		foreign := e.commit(rogue, "rogue2.txt", "again\n")
		e.run(rogue, "push", "-q", "origin", "main")

		res := e.sync(slug, false)
		if ev := eventState(res, "mirror-b"); ev.State != "diverged" {
			t.Fatalf("expected divergence, got %+v", res)
		}
		res = e.sync(slug, true)
		if ev := eventState(res, "mirror-b"); ev.State != "synced" || ev.Changed != 1 {
			t.Fatalf("overwrite did not sync: %+v", res)
		}
		if e.refs(remB)["refs/heads/main"] != e.refs(hubDir)["refs/heads/main"] {
			t.Fatal("mirror-b main not overwritten")
		}
		rb := e.remoteByTarget(slug, "mirror-b")
		suffix := fmt.Sprintf("/remotes/%d/refs/heads/main", rb.ID)
		found := false
		for ref, sha := range e.allRefs(hubDir) {
			if strings.HasPrefix(ref, "refs/backup/") && strings.HasSuffix(ref, suffix) && sha == foreign {
				found = true
			}
		}
		if !found {
			t.Fatalf("foreign commit %s not backed up: %v", foreign, e.allRefs(hubDir))
		}
	})

	t.Run("only branches and tags may be pushed", func(t *testing.T) {
		out, err := gitCmd(work, "push", hub, "HEAD:refs/custom/x")
		if err == nil {
			t.Fatalf("push to refs/custom succeeded:\n%s", out)
		}
		mustContain(t, out, "only branches (refs/heads/*) and tags (refs/tags/*) can be pushed")
	})

	t.Run("file size limit follows the strictest mirror", func(t *testing.T) {
		e.do(http.MethodPatch, "/api/targets/mirror-a", map[string]any{"max_file_size": 1024}, nil)
		before := e.refs(hubDir)["refs/heads/main"]
		e.commit(work, "big.bin", strings.Repeat("x", 4096))
		out, err := gitCmd(work, "push", hub, "main")
		if err == nil {
			t.Fatalf("oversized push succeeded:\n%s", out)
		}
		mustContain(t, out, "big.bin (4.0 KiB)", "files larger than 1.0 KiB are rejected")
		if e.refs(hubDir)["refs/heads/main"] != before {
			t.Fatal("hub accepted the oversized push")
		}
		e.run(work, "reset", "-q", "--hard", "HEAD~1")
		e.do(http.MethodPatch, "/api/targets/mirror-a", map[string]any{"max_file_size": 0}, nil)
	})

	t.Run("large pushes stream through chunked requests", func(t *testing.T) {
		blob := make([]byte, 3<<20)
		rand.Read(blob)
		if err := os.WriteFile(filepath.Join(work, "large.bin"), blob, 0o644); err != nil {
			t.Fatal(err)
		}
		e.run(work, "add", "large.bin")
		e.run(work, "commit", "-q", "-m", "large")
		out := e.run(work, "push", hub, "main")
		mustContain(t, out, "[ok]       mirror-a:demo", "[ok]       mirror-b:demo")
		sameRefs(t, "mirror-a", e.refs(remA), e.refs(hubDir))
	})

	t.Run("lfs requests get an explanation", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "http://"+e.hubAddr+"/"+slug+".git/info/lfs/objects/batch", strings.NewReader("{}"))
		req.SetBasicAuth("x", dev.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotImplemented || !strings.Contains(string(body), "not supported") {
			t.Fatalf("lfs response %d %s", resp.StatusCode, body)
		}
	})

	t.Run("push option gitrepo.wait=0 returns immediately", func(t *testing.T) {
		tip := e.commit(work, "async.txt", "async\n")
		out := e.run(work, "push", "-o", "gitrepo.wait=0", hub, "main")
		mustContain(t, out, "sync to 2 mirror(s) queued")
		deadline := time.Now().Add(30 * time.Second)
		for e.refs(remA)["refs/heads/main"] != tip || e.refs(remB)["refs/heads/main"] != tip {
			if time.Now().After(deadline) {
				t.Fatal("background sync did not finish")
			}
			time.Sleep(100 * time.Millisecond)
		}
	})

	t.Run("failures are recorded with backoff and recover", func(t *testing.T) {
		rb := e.remoteByTarget(slug, "mirror-b")
		path := fmt.Sprintf("/api/repos/%s/remotes/%d", slug, rb.ID)
		e.do(http.MethodPatch, path, map[string]any{"url": e.remoteURL + "/b/missing.git"}, nil)
		res := e.sync(slug, false)
		if ev := eventState(res, "mirror-b"); ev.State != "failed" || !strings.Contains(ev.Error, "ls-remote") {
			t.Fatalf("expected failure: %+v", res)
		}
		rb = e.remoteByTarget(slug, "mirror-b")
		// The PATCH itself queues a sync, so one or two failed attempts may be recorded.
		if rb.State != "failed" || rb.ConsecutiveFailures < 1 || rb.NextRetryAt == nil || rb.LastError == "" {
			t.Fatalf("failure not recorded: %+v", rb)
		}
		e.do(http.MethodPatch, path, map[string]any{"url": ""}, nil)
		res = e.sync(slug, false)
		if ev := eventState(res, "mirror-b"); ev.State != "synced" {
			t.Fatalf("did not recover: %+v", res)
		}
		rb = e.remoteByTarget(slug, "mirror-b")
		if rb.ConsecutiveFailures != 0 || rb.NextRetryAt != nil || rb.LastError != "" {
			t.Fatalf("recovery not recorded: %+v", rb)
		}
	})

	t.Run("paused mirrors are checked but not pushed", func(t *testing.T) {
		ra := e.remoteByTarget(slug, "mirror-a")
		path := fmt.Sprintf("/api/repos/%s/remotes/%d", slug, ra.ID)
		e.do(http.MethodPatch, path, map[string]any{"enabled": false}, nil)
		tip := e.commit(work, "paused.txt", "paused\n")
		out := e.run(work, "push", hub, "main")
		mustContain(t, out, "[paused]   mirror-a:demo", "[ok]       mirror-b:demo")
		if e.refs(remA)["refs/heads/main"] == tip {
			t.Fatal("paused mirror was pushed")
		}
		e.do(http.MethodPatch, path, map[string]any{"enabled": true}, nil)
		if ev := eventState(e.sync(slug, false), "mirror-a"); ev.State != "synced" {
			t.Fatal("resumed mirror did not sync")
		}
		if e.refs(remA)["refs/heads/main"] != tip {
			t.Fatal("resumed mirror is behind")
		}
	})

	t.Run("repository without mirrors keeps the push on the hub", func(t *testing.T) {
		e.do(http.MethodPost, "/api/repos", map[string]any{"slug": "solo/app", "bindings": false}, nil)
		out := e.run(work, "push", e.hubURL(dev.Token, "solo/app"), "main")
		mustContain(t, out, "no mirrors configured for solo/app")
	})
}

func TestConsoleRoutes(t *testing.T) {
	e := newEnv(t)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Get("http://" + e.hubAddr + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/ui/" {
		t.Fatalf("GET / = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, err = http.Get("http://" + e.hubAddr + "/ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/app.js = %d", resp.StatusCode)
	}
	// "ui" cannot be taken by a repository.
	var out map[string]any
	if err := e.api.Do(t.Context(), http.MethodPost, "/api/repos", map[string]any{"slug": "ui/app"}, &out); err == nil {
		t.Fatal("created a repository under the reserved org ui")
	}
}
