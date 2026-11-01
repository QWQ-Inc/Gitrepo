package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/client"
	"github.com/qwq-inc/gitrepo/internal/config"
	"github.com/qwq-inc/gitrepo/internal/lfs"
)

func eventually(t *testing.T, what string, within time.Duration, ok func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for !ok() {
		if time.Since(start) > within {
			t.Fatalf("%s: not within %s", what, within)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return time.Since(start)
}

func TestEdgeNode(t *testing.T) {
	e := newEnv(t)
	var edgeTok, dev, reader client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "edge-tokyo", "scopes": []string{"edge"}}, &edgeTok)
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "reader", "scopes": []string{"read"}}, &reader)
	hub := e.hubURL(dev.Token, "acme/app")
	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	e.commit(work, "a.txt", "1\n")
	e.run(work, "tag", "v1")
	e.run(work, "push", "-q", hub, "main", "v1")

	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "edge")
	cfg.Git.SelfPath = gitrepoBin
	cfg.SecretKeyEnv = e.cfg.SecretKeyEnv
	cfg.AdminTokenEnv = "RH_TEST_EDGE_ADMIN"
	t.Setenv("RH_TEST_EDGE_ADMIN", "edge-admin-secret")
	cfg.Edge.Upstream = "http://" + e.hubAddr
	cfg.Edge.TokenEnv = "RH_TEST_EDGE_TOKEN"
	t.Setenv("RH_TEST_EDGE_TOKEN", edgeTok.Token)
	_, edgeAddr := startServer(t, cfg, slog.New(slog.NewTextHandler(e.logs, nil)))
	edgeURL := func(tok, slug string) string {
		if tok == "" {
			return "http://" + edgeAddr + "/" + slug + ".git"
		}
		return "http://x:" + tok + "@" + edgeAddr + "/" + slug + ".git"
	}
	lsRemote := func(url string) (map[string]string, error) {
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
	hubRefs := func(slug string) map[string]string { return e.refs(e.hubRepo(slug)) }
	same := func(a, b map[string]string) bool {
		if len(a) != len(b) {
			return false
		}
		for k, v := range a {
			if b[k] != v {
				return false
			}
		}
		return true
	}

	t.Run("the edge loads the hub and serves clones with the hub's tokens", func(t *testing.T) {
		eventually(t, "edge copy", 15*time.Second, func() bool {
			refs, err := lsRemote(edgeURL(reader.Token, "acme/app"))
			return err == nil && same(refs, hubRefs("acme/app"))
		})
		clone := filepath.Join(t.TempDir(), "clone")
		e.run("", "clone", "-q", edgeURL(reader.Token, "acme/app"), clone)
		if b, _ := os.ReadFile(filepath.Join(clone, "a.txt")); string(b) != "1\n" {
			t.Fatalf("clone content: %q", b)
		}
		if _, err := lsRemote(edgeURL("", "acme/app")); err == nil {
			t.Fatal("anonymous read of a private repository on the edge")
		}
		if _, err := lsRemote(edgeURL("grp_bogus", "acme/app")); err == nil {
			t.Fatal("unknown token accepted by the edge")
		}
	})

	t.Run("public repositories are readable anonymously", func(t *testing.T) {
		e.do(http.MethodPost, "/api/repos", map[string]any{"slug": "acme/pub", "visibility": "public"}, nil)
		e.run(work, "push", "-q", e.hubURL(dev.Token, "acme/pub"), "main")
		eventually(t, "public repo on the edge", 10*time.Second, func() bool {
			refs, err := lsRemote(edgeURL("", "acme/pub"))
			return err == nil && refs["refs/heads/main"] != ""
		})
	})

	t.Run("a push to the hub reaches the edge within moments", func(t *testing.T) {
		sha := e.commit(work, "a.txt", "2\n")
		e.run(work, "push", "-q", hub, "main")
		took := eventually(t, "new commit on the edge", 5*time.Second, func() bool {
			refs, err := lsRemote(edgeURL(dev.Token, "acme/app"))
			return err == nil && refs["refs/heads/main"] == sha
		})
		t.Logf("propagation took %s", took)
	})

	t.Run("a push to the edge is redirected to the hub", func(t *testing.T) {
		sha := e.commit(work, "a.txt", "3\n")
		helper := "!f() { echo username=x; echo password=" + dev.Token + "; }; f"
		out, err := gitCmd(work, "-c", "credential.helper=", "-c", "credential.helper="+helper, "push", edgeURL("", "acme/app"), "main")
		if err != nil {
			t.Fatalf("push via the edge: %v\n%s", err, out)
		}
		if hubRefs("acme/app")["refs/heads/main"] != sha {
			t.Fatal("the push did not land on the hub")
		}
		eventually(t, "pushed commit back on the edge", 5*time.Second, func() bool {
			refs, err := lsRemote(edgeURL(dev.Token, "acme/app"))
			return err == nil && refs["refs/heads/main"] == sha
		})
	})

	t.Run("LFS downloads are served by the edge, uploads go to the hub", func(t *testing.T) {
		content := []byte("large object on the hub\n")
		oid := lfsOID(content)
		putLFS(t, e.hubRepo("acme/app"), oid, content)
		batch := func(op string, objs []map[string]any) map[string]any {
			body, _ := json.Marshal(map[string]any{"operation": op, "objects": objs})
			req, _ := http.NewRequest(http.MethodPost, edgeURL("", "acme/app")+"/info/lfs/objects/batch", bytes.NewReader(body))
			req.SetBasicAuth("x", dev.Token)
			req.Header.Set("Accept", "application/vnd.git-lfs+json")
			req.Header.Set("Content-Type", "application/vnd.git-lfs+json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var out map[string]any
			json.NewDecoder(resp.Body).Decode(&out)
			if resp.StatusCode != 200 {
				t.Fatalf("batch %s: %s %v", op, resp.Status, out)
			}
			return out
		}
		action := func(out map[string]any, name string) (string, map[string]any) {
			obj := out["objects"].([]any)[0].(map[string]any)
			acts, _ := obj["actions"].(map[string]any)
			a, _ := acts[name].(map[string]any)
			if a == nil {
				t.Fatalf("no %s action: %v", name, obj)
			}
			h, _ := a["header"].(map[string]any)
			return a["href"].(string), h
		}
		href, hdr := action(batch("download", []map[string]any{{"oid": oid, "size": len(content)}}), "download")
		if !strings.Contains(href, edgeAddr) {
			t.Fatalf("download link does not point to the edge: %s", href)
		}
		req, _ := http.NewRequest(http.MethodGet, href, nil)
		for k, v := range hdr {
			req.Header.Set(k, v.(string))
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !bytes.Equal(got, content) {
			t.Fatalf("download via the edge: %s %q", resp.Status, got)
		}
		if _, err := os.Stat(lfs.ObjectPath(filepath.Join(cfg.ReposDir(), "acme", "app.git"), oid)); err != nil {
			t.Fatal("the object was not cached on the edge")
		}

		upContent := []byte("uploaded through the edge\n")
		upOID := lfsOID(upContent)
		href, hdr = action(batch("upload", []map[string]any{{"oid": upOID, "size": len(upContent)}}), "upload")
		if !strings.Contains(href, e.hubAddr) {
			t.Fatalf("upload link does not point to the hub: %s", href)
		}
		req, _ = http.NewRequest(http.MethodPut, href, bytes.NewReader(upContent))
		for k, v := range hdr {
			req.Header.Set(k, v.(string))
		}
		if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode >= 300 {
			t.Fatalf("upload: %v %v", err, resp)
		}
		if _, err := os.Stat(lfs.ObjectPath(e.hubRepo("acme/app"), upOID)); err != nil {
			t.Fatal("the upload did not reach the hub")
		}
	})

	t.Run("archives come from the edge's copy", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "http://"+edgeAddr+"/archive/acme/app/v1.tar.gz", nil)
		req.SetBasicAuth("x", dev.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/gzip" {
			t.Fatalf("archive: %s", resp.Status)
		}
	})

	t.Run("revoked tokens and deleted repositories disappear from the edge", func(t *testing.T) {
		var list []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		}
		e.do(http.MethodGet, "/api/tokens", nil, &list)
		for _, tk := range list {
			if tk.Name == "reader" {
				e.do(http.MethodDelete, "/api/tokens/"+strconv.FormatInt(tk.ID, 10), nil, nil)
			}
		}
		eventually(t, "revocation on the edge", 5*time.Second, func() bool {
			_, err := lsRemote(edgeURL(reader.Token, "acme/app"))
			return err != nil
		})
		e.do(http.MethodDelete, "/api/repos/acme/pub", nil, nil)
		eventually(t, "deleted repository on the edge", 5*time.Second, func() bool {
			_, err := lsRemote(edgeURL("", "acme/pub"))
			_, statErr := os.Stat(filepath.Join(cfg.ReposDir(), "acme", "pub.git"))
			return err != nil && os.IsNotExist(statErr)
		})
	})

	t.Run("an edge token reads, but cannot administer the hub; others cannot replicate", func(t *testing.T) {
		if err := client.New("http://"+e.hubAddr, edgeTok.Token).Do(t.Context(), http.MethodGet, "/api/targets", nil, nil); err == nil {
			t.Fatal("an edge token used the admin API")
		}
		if err := client.New("http://"+e.hubAddr, dev.Token).Do(t.Context(), http.MethodGet, "/api/edge/state", nil, nil); err == nil {
			t.Fatal("a create token read the replication state")
		}
		out, err := gitCmd(work, "push", "http://x:"+edgeTok.Token+"@"+e.hubAddr+"/acme/app.git", "HEAD:refs/heads/x")
		if err == nil || !strings.Contains(out, "no write access") {
			t.Fatalf("an edge token pushed to the hub (or failed for another reason): %v\n%s", err, out)
		}
	})

	t.Run("the edge has no admin API but reports replication metrics", func(t *testing.T) {
		resp, err := http.Get("http://" + edgeAddr + "/api/repos")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatalf("edge API: %s", resp.Status)
		}
		req, _ := http.NewRequest(http.MethodGet, "http://"+edgeAddr+"/metrics", nil)
		req.Header.Set("Authorization", "Bearer "+adminToken) // the hub's admin token is not replicated
		if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 401 {
			t.Fatalf("the hub's admin token worked on the edge: %v %v", err, resp)
		}
		req.Header.Set("Authorization", "Bearer edge-admin-secret")
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), "gitrepo_edge_ready 1") || !strings.Contains(string(body), "gitrepo_edge_upstream_ok 1") {
			t.Fatalf("metrics: %s %s", resp.Status, body)
		}
	})
}
