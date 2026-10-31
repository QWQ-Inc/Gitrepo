package server_test

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/qwq-inc/gitrepo/internal/client"
	"github.com/qwq-inc/gitrepo/internal/config"
)

// A git archive that dies mid-stream must not produce a valid file.
func TestTruncatedArchiveIsAborted(t *testing.T) {
	bin := t.TempDir()
	wrapper := filepath.Join(bin, "git")
	os.WriteFile(wrapper, []byte("#!/bin/sh\nfor a in \"$@\"; do\n  if [ \"$a\" = archive ]; then git \"$@\" | head -c 200; exit 1; fi\ndone\nexec git \"$@\"\n"), 0o755)
	e := newEnv(t, func(c *config.Config) { c.Git.Binary = wrapper })
	var dev client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	e.do(http.MethodPost, "/api/repos", map[string]any{"slug": "pub/app", "visibility": "public", "bindings": false}, nil)
	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	big := make([]byte, 256<<10)
	for i := range big {
		big[i] = byte(i * 7)
	}
	os.WriteFile(filepath.Join(work, "big.bin"), big, 0o644)
	e.run(work, "add", "big.bin")
	e.run(work, "commit", "-q", "-m", "big")
	e.run(work, "push", "-q", e.hubURL(dev.Token, "pub/app"), "main")
	sha := e.refs(e.hubRepo("pub/app"))["refs/heads/main"]

	resp, err := http.Get("http://" + e.hubAddr + "/archive/pub/app/" + sha + ".tar.gz")
	if err != nil {
		return // aborted before the headers were sent
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatalf("a truncated archive was delivered as a complete response (%d)", resp.StatusCode)
	}
}
