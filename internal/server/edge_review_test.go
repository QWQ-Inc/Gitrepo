package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/client"
	"github.com/qwq-inc/gitrepo/internal/config"
	"github.com/qwq-inc/gitrepo/internal/server"
)

type edgeNode struct {
	addr string
	srv  *server.Server
	cfg  *config.Config
}

// startEdgeNode runs an edge of the env's hub (upstream overrides the hub
// URL, e.g. a dead port).
func startEdgeNode(t *testing.T, e *env, token, dataDir, upstream, gitBin string) *edgeNode {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = dataDir
	cfg.Git.SelfPath = gitrepoBin
	if gitBin != "" {
		cfg.Git.Binary = gitBin
	}
	cfg.SecretKeyEnv = e.cfg.SecretKeyEnv
	cfg.AdminTokenEnv = "RH_TEST_EDGE_ADMIN_UNSET"
	cfg.Edge.Upstream = upstream
	if upstream == "" {
		cfg.Edge.Upstream = "http://" + e.hubAddr
	}
	cfg.Edge.TokenEnv = "RH_TEST_EDGE_TOKEN"
	t.Setenv("RH_TEST_EDGE_TOKEN", token)
	srv, addr := startServer(t, cfg, slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})).With("node", "edge"))
	return &edgeNode{addr: addr, srv: srv, cfg: cfg}
}

func (n *edgeNode) url(tok, slug string) string {
	if tok == "" {
		return "http://" + n.addr + "/" + slug + ".git"
	}
	return "http://x:" + tok + "@" + n.addr + "/" + slug + ".git"
}

func lsRefs(url string) (map[string]string, error) {
	out, err := gitCmd("", "ls-remote", url)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if sha, ref, ok := strings.Cut(l, "\t"); ok && ref != "HEAD" {
			refs[ref] = sha
		}
	}
	return refs, nil
}

func fetchBySHA(url, sha string) error {
	probe, _ := os.MkdirTemp("", "probe")
	defer os.RemoveAll(probe)
	gitCmd("", "init", "-q", probe)
	_, err := gitCmd(probe, "-c", "protocol.version=2", "fetch", "-q", url, sha)
	return err
}

func TestEdgeReviewFixes(t *testing.T) {
	e := newEnv(t)
	var edgeTok, dev, reader client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "edge", "scopes": []string{"edge"}}, &edgeTok)
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "reader", "scopes": []string{"read"}}, &reader)
	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	e.commit(work, "a.txt", "1\n")
	e.run(work, "push", "-q", e.hubURL(dev.Token, "acme/app"), "main")

	// The edge's git stalls fetches of acme/big, to hold a worker busy.
	wrapper := filepath.Join(t.TempDir(), "git")
	os.WriteFile(wrapper, []byte(`#!/bin/sh
case " $* " in *"acme/big.git"*) sleep 3 ;; esac
exec git "$@"
`), 0o755)
	edgeData := filepath.Join(t.TempDir(), "edge")
	n := startEdgeNode(t, e, edgeTok.Token, edgeData, "", wrapper)
	eventually(t, "edge ready", 15*time.Second, func() bool {
		_, err := lsRefs(n.url(reader.Token, "acme/app"))
		return err == nil
	})

	t.Run("commits purged on the hub are no longer fetchable from the edge", func(t *testing.T) {
		secret := e.commit(work, "secret.txt", "AWS_SECRET=x\n")
		e.run(work, "push", "-q", e.hubURL(dev.Token, "acme/app"), "main")
		eventually(t, "secret on edge", 5*time.Second, func() bool { return fetchBySHA(n.url(dev.Token, "acme/app"), secret) == nil })
		e.run(work, "reset", "-q", "--hard", "HEAD~1")
		e.run(work, "push", "-q", "--force", e.hubURL(dev.Token, "acme/app"), "main")
		ageObjects(t, e.hubRepo("acme/app"))
		e.do(http.MethodPost, "/api/repos/acme/app/backups/purge", map[string]any{"all": true, "lfs": true}, nil)
		if fetchBySHA(e.hubURL(dev.Token, "acme/app"), secret) == nil {
			t.Fatal("setup: the hub still serves the purged commit")
		}
		eventually(t, "purged commit gone from the edge", 10*time.Second, func() bool {
			refs, err := lsRefs(n.url(dev.Token, "acme/app"))
			return err == nil && refs["refs/heads/main"] != "" && fetchBySHA(n.url(dev.Token, "acme/app"), secret) != nil
		})
	})

	t.Run("a private repository re-created as public never exposes the old one", func(t *testing.T) {
		// A separate edge that is down while the slug is deleted and
		// re-created, so it only ever sees the final state.
		xData := filepath.Join(t.TempDir(), "edge-x")
		x := startEdgeNode(t, e, edgeTok.Token, xData, "", "")
		e.do(http.MethodPost, "/api/repos", map[string]any{"slug": "acme/x"}, nil)
		old := filepath.Join(t.TempDir(), "old")
		e.run("", "init", "-q", "-b", "main", old)
		oldSHA := e.commit(old, "private.txt", "private\n")
		e.run(old, "push", "-q", e.hubURL(dev.Token, "acme/x"), "main")
		eventually(t, "private copy on edge", 5*time.Second, func() bool {
			refs, err := lsRefs(x.url(dev.Token, "acme/x"))
			return err == nil && refs["refs/heads/main"] == oldSHA
		})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		x.srv.Shutdown(ctx)
		cancel()
		e.do(http.MethodDelete, "/api/repos/acme/x", nil, nil)
		e.do(http.MethodPost, "/api/repos", map[string]any{"slug": "acme/x", "visibility": "public"}, nil)
		fresh := filepath.Join(t.TempDir(), "fresh")
		e.run("", "init", "-q", "-b", "main", fresh)
		newSHA := e.commit(fresh, "public.txt", "public\n")
		e.run(fresh, "push", "-q", e.hubURL(dev.Token, "acme/x"), "main")

		x = startEdgeNode(t, e, edgeTok.Token, xData, "", "")
		eventually(t, "public copy on edge", 10*time.Second, func() bool {
			refs, err := lsRefs(x.url("", "acme/x"))
			if err == nil && refs["refs/heads/main"] == oldSHA {
				t.Fatal("the old private branch was served anonymously")
			}
			return err == nil && refs["refs/heads/main"] == newSHA
		})
		if fetchBySHA(x.url("", "acme/x"), oldSHA) == nil {
			t.Fatal("an old private commit is fetchable anonymously by SHA")
		}
	})

	t.Run("revocations apply while a long fetch runs", func(t *testing.T) {
		big := filepath.Join(t.TempDir(), "big")
		e.run("", "init", "-q", "-b", "main", big)
		e.commit(big, "b.txt", "b\n")
		e.run(big, "push", "-q", e.hubURL(dev.Token, "acme/big"), "main") // the edge's fetch now sleeps 3s
		time.Sleep(300 * time.Millisecond)
		var list []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		}
		e.do(http.MethodGet, "/api/tokens", nil, &list)
		for _, tk := range list {
			if tk.Name == "reader" {
				e.do(http.MethodDelete, "/api/tokens/"+itoa(tk.ID), nil, nil)
			}
		}
		took := eventually(t, "revocation on the edge", 2*time.Second, func() bool {
			_, err := lsRefs(n.url(reader.Token, "acme/app"))
			return err != nil
		})
		t.Logf("revocation reached the edge in %s during a 3s fetch", took)
	})

	t.Run("LFS uploads for a repository created by the push go to the hub", func(t *testing.T) {
		// git push: the redirected first request creates the repository on the hub ...
		req, _ := http.NewRequest(http.MethodGet, n.url("", "acme/lfsnew")+"/info/refs?service=git-receive-pack", nil)
		req.SetBasicAuth("x", dev.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		// ... then git-lfs's pre-push asks the edge (the remote URL) for upload links.
		content := []byte("big file\n")
		body, _ := json.Marshal(map[string]any{"operation": "upload", "objects": []map[string]any{{"oid": lfsOID(content), "size": len(content)}}})
		req, _ = http.NewRequest(http.MethodPost, n.url("", "acme/lfsnew")+"/info/lfs/objects/batch", bytes.NewReader(body))
		req.SetBasicAuth("x", dev.Token)
		req.Header.Set("Content-Type", "application/vnd.git-lfs+json")
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(out), e.hubAddr) {
			t.Fatalf("upload batch through the edge: %s %s", resp.Status, out)
		}
	})

	t.Run("one LFS object is fetched from the hub once, without holding the batch", func(t *testing.T) {
		content := bytes.Repeat([]byte("x"), 1<<20)
		oid := lfsOID(content)
		putLFS(t, e.hubRepo("acme/app"), oid, content)
		body, _ := json.Marshal(map[string]any{"operation": "download", "objects": []map[string]any{{"oid": oid, "size": len(content)}}})
		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req, _ := http.NewRequest(http.MethodPost, n.url("", "acme/app")+"/info/lfs/objects/batch", bytes.NewReader(body))
				req.SetBasicAuth("x", dev.Token)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				var out struct {
					Objects []struct {
						Actions struct {
							Download struct {
								Href   string            `json:"href"`
								Header map[string]string `json:"header"`
							} `json:"download"`
						} `json:"actions"`
					} `json:"objects"`
				}
				json.NewDecoder(resp.Body).Decode(&out)
				resp.Body.Close()
				d := out.Objects[0].Actions.Download
				get, _ := http.NewRequest(http.MethodGet, d.Href, nil)
				for k, v := range d.Header {
					get.Header.Set(k, v)
				}
				r2, err := http.DefaultClient.Do(get)
				if err != nil {
					t.Error(err)
					return
				}
				got, _ := io.ReadAll(r2.Body)
				r2.Body.Close()
				if !bytes.Equal(got, content) {
					t.Errorf("download: %s, %d bytes", r2.Status, len(got))
				}
			}()
		}
		wg.Wait()
		n := 0
		for _, line := range strings.Split(e.logs.String(), "\n") {
			if strings.Contains(line, "path=/acme/app.git/info/lfs/objects/"+oid+" status=200") && !strings.Contains(line, "node=edge") {
				n++ // served by the hub, not by the edge
			}
		}
		if n != 1 {
			t.Fatalf("the hub served the object %d times", n)
		}
	})

	t.Run("edge tokens cover everything and never receive the admin hash", func(t *testing.T) {
		if err := e.api.Do(t.Context(), http.MethodPost, "/api/tokens", map[string]any{"name": "edge2", "scopes": []string{"edge"}, "repos": []string{"acme/*"}}, nil); err == nil {
			t.Fatal("a repository-restricted edge token was created")
		}
		req, _ := http.NewRequest(http.MethodGet, "http://"+e.hubAddr+"/api/edge/state", nil)
		req.Header.Set("Authorization", "Bearer "+edgeTok.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(raw), "admin_hash") || strings.Contains(string(raw), adminTokenHash()) {
			t.Fatal("the admin token hash was replicated")
		}
	})

	t.Run("a restarted edge serves its copy while the hub is unreachable", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		n.srv.Shutdown(ctx)
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		dead := "http://" + ln.Addr().String()
		ln.Close()
		n2 := startEdgeNode(t, e, edgeTok.Token, edgeData, dead, "")
		refs, err := lsRefs(n2.url(dev.Token, "acme/app"))
		if err != nil || refs["refs/heads/main"] == "" {
			t.Fatalf("restarted edge with the hub down: %v %v", refs, err)
		}
	})

	t.Run("the hub shuts down promptly with an edge long-polling", func(t *testing.T) {
		startEdgeNode(t, e, edgeTok.Token, filepath.Join(t.TempDir(), "edge3"), "", "")
		time.Sleep(500 * time.Millisecond) // the edge is now waiting on the hub
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		start := time.Now()
		if err := e.srv.Shutdown(ctx); err != nil && !strings.Contains(err.Error(), "closed") {
			t.Logf("shutdown: %v", err)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("hub shutdown took %s", d)
		}
	})
}

func adminTokenHash() string {
	return sha256hex([]byte(adminToken))
}
