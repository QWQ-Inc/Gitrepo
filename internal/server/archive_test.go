package server_test

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwq-inc/gitrepo/internal/client"
)

func TestSourceArchives(t *testing.T) {
	e := newEnv(t)
	var dev client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	sha := e.commit(work, "main.go", "package main\n")
	e.run(work, "checkout", "-q", "-b", "feature/x")
	e.commit(work, "feature.go", "package main // feature\n")
	e.run(work, "checkout", "-q", "main")
	e.do(http.MethodPost, "/api/repos", map[string]any{"slug": "pub/app", "visibility": "public", "bindings": false}, nil)
	e.run(work, "push", "-q", e.hubURL(dev.Token, "pub/app"), "main", "feature/x")
	e.run(work, "push", "-q", e.hubURL(dev.Token, "priv/app"), "main")

	get := func(path, token string, hdr ...string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, "http://"+e.hubAddr+path, nil)
		if token != "" {
			req.SetBasicAuth("x", token)
		}
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	files := func(resp *http.Response) []string {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(gz)
		var names []string
		for {
			h, err := tr.Next()
			if err == io.EOF {
				return names
			}
			if err != nil {
				t.Fatal(err)
			}
			names = append(names, h.Name)
		}
	}

	t.Run("a public archive by SHA is immutable and anonymous", func(t *testing.T) {
		resp := get("/archive/pub/app/"+sha+".tar.gz", "")
		if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Fatalf("%d %v", resp.StatusCode, resp.Header)
		}
		names := strings.Join(files(resp), ",")
		if !strings.Contains(names, "app-"+sha[:12]+"/main.go") || strings.Contains(names, "feature.go") {
			t.Fatalf("archive content: %s", names)
		}
		if r := get("/archive/pub/app/"+sha+".tar.gz", "", "If-None-Match", resp.Header.Get("ETag")); r.StatusCode != http.StatusNotModified {
			t.Fatalf("conditional request: %d", r.StatusCode)
		}
	})

	t.Run("branch names (with slashes) work but are not cached", func(t *testing.T) {
		resp := get("/archive/pub/app/feature/x.zip", "")
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/zip" || resp.Header.Get("Cache-Control") != "private, no-cache" {
			t.Fatalf("%d %v", resp.StatusCode, resp.Header)
		}
	})

	t.Run("private archives need read access", func(t *testing.T) {
		if r := get("/archive/priv/app/main.tar.gz", ""); r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("anonymous: %d", r.StatusCode)
		}
		if r := get("/archive/priv/app/main.tar.gz", dev.Token); r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Cache-Control"), "private") {
			t.Fatalf("with token: %d %v", r.StatusCode, r.Header)
		}
	})

	t.Run("commits outside branches and tags are not served", func(t *testing.T) {
		hidden, err := e.git.Run(t.Context(), e.hubRepo("pub/app"), "commit-tree", "4b825dc642cb6eb9a060e54bf8d69288fbee4904", "-m", "backup only")
		if err != nil {
			t.Fatal(err)
		}
		id := strings.TrimSpace(string(hidden))
		e.git.UpdateRefs(t.Context(), e.hubRepo("pub/app"), []string{"update refs/backup/20260101T000000.000Z/refs/heads/old " + id})
		for _, rev := range []string{id, "--output=x", "main:main.go", "HEAD~1"} {
			if r := get("/archive/pub/app/"+rev+".tar.gz", ""); r.StatusCode != http.StatusNotFound {
				t.Errorf("%s: %d", rev, r.StatusCode)
			}
		}
	})
}
