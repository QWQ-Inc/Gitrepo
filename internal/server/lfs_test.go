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
	"testing"

	"github.com/qwq-inc/gitrepo/internal/client"
	"github.com/qwq-inc/gitrepo/internal/gitserve"
	"github.com/qwq-inc/gitrepo/internal/lfs"
)

// newLFSPlatform is a fake hosting platform with Git LFS: smart HTTP plus
// the LFS API, both behind basic auth (mirror/s3cret).
func newLFSPlatform(t *testing.T) (url, dir string) {
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
