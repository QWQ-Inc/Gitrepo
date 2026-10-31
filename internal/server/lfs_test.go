package server_test

import (
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/client"
	"github.com/qwq-inc/gitrepo/internal/gitserve"
	"github.com/qwq-inc/gitrepo/internal/lfs"
)

// newLFSPlatform is a fake hosting platform with Git LFS: smart HTTP plus
// the LFS API, both behind basic auth (mirror/s3cret).
func newLFSPlatform(t *testing.T, uploadPackDelay ...time.Duration) (url, dir string) {
	dir = t.TempDir()
	signer := lfs.NewSigner([]byte("fake platform key"))
	basic := func(r *http.Request) bool {
		u, p, ok := r.BasicAuth()
		return ok && u == userA && p == passA
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if repoPath, rest, ok := strings.Cut(r.URL.Path, ".git/info/lfs/"); ok {
			slug := strings.TrimPrefix(repoPath, "/")
			ls := &lfs.Server{Signer: signer, Auth: func(r *http.Request, _ string, _ bool) error {
				if !basic(r) {
					return lfs.ErrUnauthorized
				}
				return nil
			}}
			ls.Serve(w, r, lfs.Repo{Slug: slug, Dir: filepath.Join(dir, slug+".git")}, rest)
			return
		}
		if !basic(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if len(uploadPackDelay) > 0 && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-upload-pack") {
			time.Sleep(uploadPackDelay[0])
		}
		gitserve.RunBackend(w, r, "git", r.URL.Path, []string{"GIT_PROJECT_ROOT=" + dir, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=" + userA, "HOME=" + dir}, slog.Default())
	}))
	t.Cleanup(srv.Close)
	return srv.URL, dir
}

func TestGitLFS(t *testing.T) {
	if _, err := gitCmd("", "lfs", "version"); err != nil {
		t.Skip("git-lfs is not installed")
	}
	e := newEnv(t)
	platformURL, platformDir := newLFSPlatform(t)
	mirror := filepath.Join(platformDir, "lfs", "assets.git")
	if err := e.git.InitBare(t.Context(), mirror, "main"); err != nil {
		t.Fatal(err)
	}
	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "lfsmirror", "provider": "generic", "url_template": platformURL + "/lfs/{name}.git",
		"credential": map[string]any{"kind": "basic", "username": userA, "secret": passA},
	}, nil)
	e.do(http.MethodPost, "/api/bindings", map[string]any{"org": "media", "target": "lfsmirror"}, nil)
	var dev client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	hub := e.hubURL(dev.Token, "media/assets")
	hubDir := e.hubRepo("media/assets")

	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	e.run(work, "lfs", "install", "--local")
	e.run(work, "lfs", "track", "*.bin")
	content := make([]byte, 300<<10)
	rand.Read(content)
	os.WriteFile(filepath.Join(work, "asset.bin"), content, 0o644)
	e.run(work, "add", ".gitattributes", "asset.bin")
	e.run(work, "commit", "-q", "-m", "add asset")
	oid := strings.Fields(strings.TrimSpace(e.run(work, "lfs", "ls-files", "--long")))[0]

	t.Run("push uploads LFS objects to the hub and then to the mirror", func(t *testing.T) {
		out := e.run(work, "push", hub, "main")
		mustContain(t, out, "[ok]       lfsmirror:assets")
		if !lfs.Has(hubDir, oid, int64(len(content))) {
			t.Fatal("object not stored on the hub")
		}
		if !lfs.Has(mirror, oid, int64(len(content))) {
			var runs []client.Run
			e.do(http.MethodGet, "/api/repos/media/assets/runs", nil, &runs)
			t.Fatalf("object not uploaded to the mirror; push output:\n%s\nruns: %+v", out, runs)
		}
		var runs []client.Run
		e.do(http.MethodGet, "/api/repos/media/assets/runs", nil, &runs)
		if len(runs) == 0 || !strings.Contains(runs[0].Detail, "uploaded 1 LFS object(s)") {
			t.Fatalf("run detail: %+v", runs)
		}
	})

	t.Run("clone downloads LFS content from the hub", func(t *testing.T) {
		clone := filepath.Join(t.TempDir(), "clone")
		cmd := []string{"-c", "filter.lfs.smudge=git-lfs smudge -- %f", "-c", "filter.lfs.process=git-lfs filter-process", "-c", "filter.lfs.required=true",
			"clone", "-q", e.hubURL(dev.Token, "media/assets"), clone}
		e.run("", cmd...)
		got, err := os.ReadFile(filepath.Join(clone, "asset.bin"))
		if err != nil || string(got) != string(content) {
			t.Fatalf("cloned content differs (%d bytes, %v)", len(got), err)
		}
	})

	t.Run("a pointer whose object was never uploaded is rejected", func(t *testing.T) {
		plain := filepath.Join(t.TempDir(), "plain")
		e.run("", "clone", "-q", "-c", "filter.lfs.smudge=", "-c", "filter.lfs.process=", "-c", "filter.lfs.required=false", hub, plain)
		fake := "version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("ab", 32) + "\nsize 12345\n"
		os.WriteFile(filepath.Join(plain, "ghost.bin"), []byte(fake), 0o644)
		e.run(plain, "-c", "filter.lfs.clean=", "-c", "filter.lfs.process=", "add", "ghost.bin")
		e.run(plain, "commit", "-q", "-m", "pointer without object")
		out, err := gitCmd(plain, "push", hub, "HEAD:main")
		if err == nil {
			t.Fatalf("push with a dangling LFS pointer succeeded:\n%s", out)
		}
		mustContain(t, out, "ghost.bin: LFS object abababababab", "missing on the hub")
	})

	t.Run("a pushed .lfsconfig cannot redirect mirror credentials", func(t *testing.T) {
		var mu sync.Mutex
		var stolen []string
		evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			stolen = append(stolen, r.Method+" "+r.URL.Path+" auth="+r.Header.Get("Authorization"))
			mu.Unlock()
			w.Header().Set("LFS-Authenticate", `Basic realm="evil"`)
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer evil.Close()
		os.WriteFile(filepath.Join(work, ".lfsconfig"), []byte("[lfs]\n\turl = "+evil.URL+"/steal/info/lfs\n\tpushurl = "+evil.URL+"/steal/info/lfs\n"), 0o644)
		extra := make([]byte, 50<<10)
		rand.Read(extra)
		os.WriteFile(filepath.Join(work, "second.bin"), extra, 0o644)
		e.run(work, "add", ".lfsconfig", "second.bin")
		e.run(work, "commit", "-q", "-m", "redirect LFS")
		second := ""
		for _, line := range strings.Split(e.run(work, "lfs", "ls-files", "--long"), "\n") {
			if strings.HasSuffix(line, "second.bin") {
				second = strings.Fields(line)[0]
			}
		}
		// The client overrides the file for its own upload to the hub.
		hubLFS := strings.TrimSuffix(hub, ".git") + ".git/info/lfs"
		out := e.run(work, "-c", "lfs.url="+hubLFS, "-c", "lfs.pushurl="+hubLFS, "push", hub, "main")
		mustContain(t, out, "[ok]       lfsmirror:assets")
		mu.Lock()
		defer mu.Unlock()
		for _, req := range stolen {
			if !strings.HasSuffix(req, "auth=") {
				t.Fatalf("mirror credentials were sent to the .lfsconfig host: %v", stolen)
			}
		}
		if len(stolen) > 0 {
			t.Fatalf("the sync talked to the .lfsconfig host at all: %v", stolen)
		}
		if !lfs.Has(mirror, second, int64(len(extra))) {
			t.Fatal("object not uploaded to the real mirror")
		}
	})

	// A commit with an LFS pointer whose object was never uploaded, placed
	// on the hub behind the hooks' back (as a fetched mirror backup or old
	// history would be).
	plain := filepath.Join(t.TempDir(), "plain")
	e.run("", "init", "-q", "-b", "main", plain)
	ghost := "version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("cd", 32) + "\nsize 99\n"
	os.WriteFile(filepath.Join(plain, "ghost.bin"), []byte(ghost), 0o644)
	e.run(plain, "add", "ghost.bin")
	e.run(plain, "commit", "-q", "-m", "ghost pointer")

	t.Run("content reachable only from refs/backup is still checked on push", func(t *testing.T) {
		e.run(plain, "push", "-q", hubDir, "HEAD:refs/backup/20260101T000000.000Z/remotes/1/refs/heads/x")
		out, err := gitCmd(plain, "push", hub, "HEAD:refs/heads/smuggled")
		if err == nil {
			t.Fatalf("push of a backed-up commit with a missing LFS object succeeded:\n%s", out)
		}
		mustContain(t, out, "ghost.bin: LFS object cdcdcdcdcdcd", "missing on the hub")
	})

	t.Run("a sync never pushes pointers whose object the hub lacks", func(t *testing.T) {
		e.run(plain, "push", "-q", hubDir, "HEAD:refs/heads/legacy")
		res := e.sync("media/assets", false)
		ev := eventState(res, "lfsmirror")
		if ev.State != "failed" || !strings.Contains(ev.Error, "not stored on the hub") {
			t.Fatalf("sync with a missing LFS object: %+v", ev)
		}
		if _, ok := e.refs(mirror)["refs/heads/legacy"]; ok {
			t.Fatal("the ref with the dangling pointer reached the mirror")
		}
		e.git.UpdateRefs(t.Context(), hubDir, []string{"delete refs/heads/legacy"})
	})

	t.Run("the LFS API does not reveal which private repositories exist", func(t *testing.T) {
		post := func(path, token string) int {
			req, _ := http.NewRequest(http.MethodPost, "http://"+e.hubAddr+path, strings.NewReader(`{"operation":"download","objects":[]}`))
			if token != "" {
				req.SetBasicAuth("x", token)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			return resp.StatusCode
		}
		if a, b := post("/media/nope.git/info/lfs/objects/batch", ""), post("/media/assets.git/info/lfs/objects/batch", ""); a != http.StatusUnauthorized || b != http.StatusUnauthorized {
			t.Fatalf("anonymous: missing repo %d, private repo %d (both must be 401)", a, b)
		}
		if got := post("/media/nope.git/info/lfs/objects/batch", dev.Token); got != http.StatusNotFound {
			t.Fatalf("authenticated, missing repo: %d", got)
		}
	})

	t.Run("the strictest mirror LFS limit is enforced at upload", func(t *testing.T) {
		e.do(http.MethodPatch, "/api/targets/lfsmirror", map[string]any{"max_lfs_object_size": 1024}, nil)
		defer e.do(http.MethodPatch, "/api/targets/lfsmirror", map[string]any{"max_lfs_object_size": 0}, nil)
		req, _ := http.NewRequest(http.MethodPost, "http://"+e.hubAddr+"/media/assets.git/info/lfs/objects/batch",
			strings.NewReader(`{"operation":"upload","objects":[{"oid":"`+strings.Repeat("ef", 32)+`","size":4096},{"oid":"`+strings.Repeat("fe", 32)+`","size":512}]}`))
		req.SetBasicAuth("x", dev.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(body), "larger than the 1024-byte limit") || strings.Count(string(body), `"upload"`) != 1 {
			t.Fatalf("batch with an oversized object: %d %s", resp.StatusCode, body)
		}
	})

	t.Run("the batch API requires credentials", func(t *testing.T) {
		resp, err := http.Post("http://"+e.hubAddr+"/media/assets.git/info/lfs/objects/batch", "application/vnd.git-lfs+json",
			strings.NewReader(`{"operation":"download","objects":[{"oid":"`+oid+`","size":1}]}`))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("LFS-Authenticate") == "" {
			t.Fatalf("anonymous batch: %d %v", resp.StatusCode, resp.Header)
		}
		get, _ := http.Get("http://" + e.hubAddr + "/media/assets.git/info/lfs/objects/" + oid)
		get.Body.Close()
		if get.StatusCode != http.StatusUnauthorized {
			t.Fatalf("anonymous object download: %d", get.StatusCode)
		}
	})
}
