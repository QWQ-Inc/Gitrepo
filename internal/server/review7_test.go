package server_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/client"
	"github.com/qwq-inc/gitrepo/internal/gitexec"
	"github.com/qwq-inc/gitrepo/internal/lfs"
	"github.com/qwq-inc/gitrepo/internal/server"
	"github.com/qwq-inc/gitrepo/internal/snapshot"
)

// Fixes for the seventh (whole-system) review.

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func lfsBatch(t *testing.T, url, tok string, objs []map[string]any) (int, []struct {
	OID     string         `json:"oid"`
	Size    int64          `json:"size"`
	Actions map[string]any `json:"actions"`
	Error   *struct {
		Code int `json:"code"`
	} `json:"error"`
}) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"operation": "download", "objects": objs})
	req, _ := http.NewRequest(http.MethodPost, url+"/info/lfs/objects/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/vnd.git-lfs+json")
	if tok != "" {
		req.SetBasicAuth("x", tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Objects []struct {
			OID     string         `json:"oid"`
			Size    int64          `json:"size"`
			Actions map[string]any `json:"actions"`
			Error   *struct {
				Code int `json:"code"`
			} `json:"error"`
		} `json:"objects"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out.Objects
}

// An edge asks the hub once per client batch (not once per listed object),
// hands out links only for objects the hub has, and remembers misses; and
// an object fetched by ID alone (size unknown) is found on the hub.
func TestEdgeLFSFillIsBounded(t *testing.T) {
	e := newEnv(t)
	var edgeTok, dev client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "edge", "scopes": []string{"edge"}}, &edgeTok)
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	e.do(http.MethodPost, "/api/repos", map[string]any{"slug": "acme/pub", "visibility": "public"}, nil)
	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	e.commit(work, "a.txt", "1\n")
	e.run(work, "push", "-q", e.hubURL(dev.Token, "acme/pub"), "main")
	real := []byte("a real object\n")
	realOID := lfsOID(real)
	putLFS(t, e.hubRepo("acme/pub"), realOID, real)
	direct := []byte("fetched by id alone\n")
	directOID := lfsOID(direct)
	putLFS(t, e.hubRepo("acme/pub"), directOID, direct)

	n := startEdgeNode(t, e, edgeTok.Token, filepath.Join(t.TempDir(), "edge"), "", "")
	eventually(t, "edge ready", 15*time.Second, func() bool {
		_, err := lsRefs(n.url("", "acme/pub"))
		return err == nil
	})
	hubBatches := func() int {
		c := 0
		for _, line := range strings.Split(e.logs.String(), "\n") {
			if strings.Contains(line, "path=/acme/pub.git/info/lfs/objects/batch") && !strings.Contains(line, "node=edge") {
				c++
			}
		}
		return c
	}

	objs := []map[string]any{{"oid": realOID, "size": len(real)}}
	for i := 0; i < 999; i++ {
		b := make([]byte, 32)
		rand.Read(b)
		objs = append(objs, map[string]any{"oid": hex.EncodeToString(b), "size": 1})
	}
	before := hubBatches()
	status, got := lfsBatch(t, n.url("", "acme/pub"), "", objs)
	if status != http.StatusOK || len(got) != len(objs) {
		t.Fatalf("anonymous batch: %d, %d objects", status, len(got))
	}
	links := 0
	for _, o := range got {
		if o.Actions["download"] != nil {
			links++
			if o.OID != realOID {
				t.Fatalf("a download link for an object the hub does not have: %s", o.OID)
			}
		} else if o.Error == nil || o.Error.Code != http.StatusNotFound {
			t.Fatalf("object %s: neither a link nor a 404: %+v", o.OID, o.Error)
		}
	}
	if links != 1 {
		t.Fatalf("%d download links, want 1 (the real object)", links)
	}
	time.Sleep(500 * time.Millisecond)
	if d := hubBatches() - before; d != 1 {
		t.Fatalf("the edge sent %d batch requests to the hub for one client batch, want 1", d)
	}
	// The misses are remembered: asking again costs the hub nothing.
	before = hubBatches()
	lfsBatch(t, n.url("", "acme/pub"), "", objs[1:])
	time.Sleep(300 * time.Millisecond)
	if d := hubBatches() - before; d != 0 {
		t.Fatalf("the edge asked the hub again (%d requests) about objects it just reported missing", d)
	}
	// The real object arrives on the edge in the background.
	edgeRepo := filepath.Join(n.cfg.ReposDir(), "acme", "pub.git")
	eventually(t, "object filled", 10*time.Second, func() bool { return lfs.Has(edgeRepo, realOID, int64(len(real))) })

	// A direct GET (no batch, so the edge does not know the size) works.
	resp, err := http.Get(n.url("", "acme/pub") + "/info/lfs/objects/" + directOID)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(b, direct) {
		t.Fatalf("direct download from the edge: %d %q", resp.StatusCode, b)
	}
	// On the hub, size 0 in a download batch means "unknown".
	status, got = lfsBatch(t, e.hubURL("", "acme/pub"), "", []map[string]any{{"oid": realOID, "size": 0}})
	if status != http.StatusOK || len(got) != 1 || got[0].Actions["download"] == nil || got[0].Size != int64(len(real)) {
		t.Fatalf("hub batch with an unknown size: %d %+v", status, got)
	}
}

// The edge's LFS endpoints serve nothing from a copy that is not the
// current one: after a private repository was deleted and re-created as
// public, its old objects must not be downloadable anonymously.
func TestEdgeLFSRequiresCurrentCopy(t *testing.T) {
	e := newEnv(t)
	var edgeTok, dev client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "edge", "scopes": []string{"edge"}}, &edgeTok)
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	e.do(http.MethodPost, "/api/repos", map[string]any{"slug": "acme/x"}, nil)
	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	e.commit(work, "a.txt", "1\n")
	e.run(work, "push", "-q", e.hubURL(dev.Token, "acme/x"), "main")
	secret := []byte("confidential build artifact\n")
	oid := lfsOID(secret)
	putLFS(t, e.hubRepo("acme/x"), oid, secret)

	// Fetches of the acme/big* repositories take long and keep all of the
	// edge's fetch workers busy.
	wrapper := filepath.Join(t.TempDir(), "git")
	writeFile(t, wrapper, []byte("#!/bin/sh\ncase \" $* \" in *acme/big*) sleep 8 ;; esac\nexec git \"$@\"\n"))
	os.Chmod(wrapper, 0o755)
	n := startEdgeNode(t, e, edgeTok.Token, filepath.Join(t.TempDir(), "edge"), "", wrapper)
	get := func(tok string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, n.url("", "acme/x")+"/info/lfs/objects/"+oid, nil)
		if tok != "" {
			req.SetBasicAuth("x", tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	eventually(t, "object served by the edge", 15*time.Second, func() bool {
		code, body := get(dev.Token)
		return code == 200 && body == string(secret)
	})
	if code, _ := get(""); code != http.StatusUnauthorized {
		t.Fatalf("anonymous download of a private object: %d", code)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	n.srv.Shutdown(ctx)
	cancel()
	for i := 1; i <= 4; i++ {
		e.do(http.MethodPost, "/api/repos", map[string]any{"slug": "acme/big" + string(rune('0'+i))}, nil)
	}
	e.do(http.MethodDelete, "/api/repos/acme/x", nil, nil)
	e.do(http.MethodPost, "/api/repos", map[string]any{"slug": "acme/x", "visibility": "public"}, nil)
	n = startEdgeNode(t, e, edgeTok.Token, n.cfg.DataDir, "", wrapper)
	eventually(t, "edge saw the re-created repository", 10*time.Second, func() bool {
		code, _ := get("")
		return code != http.StatusUnauthorized
	})
	for i := 0; i < 5; i++ {
		code, body := get("")
		if body == string(secret) {
			t.Fatalf("the deleted private repository's LFS object was served anonymously (%d)", code)
		}
		if code != http.StatusServiceUnavailable && code != http.StatusNotFound {
			t.Fatalf("anonymous download from a copy being replaced: %d %s", code, body)
		}
		time.Sleep(200 * time.Millisecond)
	}
	// A batch is refused the same way.
	if status, _ := lfsBatch(t, n.url("", "acme/x"), "", []map[string]any{{"oid": oid, "size": len(secret)}}); status == http.StatusOK {
		eventually(t, "copy replaced", 30*time.Second, func() bool { _, err := lsRefs(n.url("", "acme/x")); return err == nil })
	} else if status != http.StatusServiceUnavailable {
		t.Fatalf("batch on a copy being replaced: %d", status)
	}
}

// A mirror-side backup is stored with its LFS files, so restoring it works
// and later syncs keep working; a backup whose LFS files are missing is
// refused, and a ref that needs missing files no longer blocks the others.
func TestBackupLFSAndHeldBackRefs(t *testing.T) {
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

	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	e.run(work, "lfs", "install", "--local")
	e.run(work, "lfs", "track", "*.bin")
	content := make([]byte, 10<<10)
	rand.Read(content)
	writeFile(t, filepath.Join(work, "asset.bin"), content)
	e.run(work, "add", ".gitattributes", "asset.bin")
	e.run(work, "commit", "-q", "-m", "add asset")
	mustContain(t, e.run(work, "push", hub, "main"), "[ok]       lfsmirror:assets")

	// Someone works on the mirror directly (with LFS on the platform), and
	// the administrator overwrites the mirror: their commit is backed up.
	mirrorURL := strings.Replace(platformURL, "http://", "http://"+userA+":"+passA+"@", 1) + "/lfs/assets.git"
	rogue := filepath.Join(t.TempDir(), "rogue")
	e.run("", "clone", "-q", mirrorURL, rogue)
	e.run(rogue, "lfs", "install", "--local")
	theirs := make([]byte, 10<<10)
	rand.Read(theirs)
	writeFile(t, filepath.Join(rogue, "theirs.bin"), theirs)
	e.run(rogue, "add", "theirs.bin")
	e.run(rogue, "commit", "-q", "-m", "work done on the mirror")
	e.run(rogue, "push", "-q", "origin", "main")
	if ev := eventState(e.sync("media/assets", true), "lfsmirror"); ev.State != "synced" {
		t.Fatalf("overwrite: %+v", ev)
	}
	var backups []struct {
		Ref    string `json:"ref"`
		Source string `json:"source"`
		SHA    string `json:"sha"`
	}
	e.do(http.MethodGet, "/api/repos/media/assets/backups", nil, &backups)
	ref, sha := "", ""
	for _, b := range backups {
		if b.Source == "remote" {
			ref, sha = b.Ref, b.SHA
		}
	}
	if ref == "" {
		t.Fatalf("no mirror backup: %+v", backups)
	}
	theirsPath := lfs.ObjectPath(e.hubRepo("media/assets"), lfsOID(theirs))
	if !lfs.Has(e.hubRepo("media/assets"), lfsOID(theirs), int64(len(theirs))) {
		t.Fatal("the backup's LFS file was not fetched with it")
	}

	// Without its LFS file the backup is refused (it could never be synced).
	saved, _ := os.ReadFile(theirsPath)
	os.Remove(theirsPath)
	err := e.api.Do(t.Context(), http.MethodPost, "/api/repos/media/assets/backups/restore", map[string]any{"backup": ref, "branch": "theirs"}, nil)
	var apiErr *client.Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict || !strings.Contains(apiErr.Message, "LFS") {
		t.Fatalf("restore of a backup whose LFS file is missing: %v", err)
	}
	// With it, restore works and the branch reaches the mirror with main.
	writeFile(t, theirsPath, saved)
	e.do(http.MethodPost, "/api/repos/media/assets/backups/restore", map[string]any{"backup": ref, "branch": "theirs"}, nil)
	writeFile(t, filepath.Join(work, "readme.txt"), []byte("unrelated\n"))
	e.run(work, "add", "readme.txt")
	e.run(work, "commit", "-q", "-m", "unrelated change")
	tip := strings.TrimSpace(e.run(work, "rev-parse", "HEAD"))
	e.run(work, "push", "-q", hub, "main")
	eventually(t, "mirror synced", 15*time.Second, func() bool {
		r := e.refs(mirror)
		return r["refs/heads/main"] == tip && r["refs/heads/theirs"] == sha
	})

	// A hub ref that needs an LFS file the hub lacks (e.g. from before the
	// hub enforced LFS) is held back; everything else still syncs.
	e.run(rogue, "checkout", "-q", "-b", "legacy")
	more := make([]byte, 4<<10)
	rand.Read(more)
	writeFile(t, filepath.Join(rogue, "more.bin"), more)
	e.run(rogue, "add", "more.bin")
	e.run(rogue, "commit", "-q", "-m", "legacy content")
	e.run(rogue, "push", "-q", e.hubRepo("media/assets"), "legacy:refs/heads/legacy") // straight into the hub's storage
	os.Remove(lfs.ObjectPath(e.hubRepo("media/assets"), lfsOID(more)))
	writeFile(t, filepath.Join(work, "readme.txt"), []byte("unrelated 2\n"))
	e.run(work, "commit", "-q", "-am", "unrelated change 2")
	tip = strings.TrimSpace(e.run(work, "rev-parse", "HEAD"))
	e.run(work, "push", "-q", hub, "main")
	ev := eventState(e.sync("media/assets", false), "lfsmirror")
	if e.refs(mirror)["refs/heads/main"] != tip {
		t.Fatalf("main did not reach the mirror while another ref lacked LFS files: %+v", ev)
	}
	if _, ok := e.refs(mirror)["refs/heads/legacy"]; ok {
		t.Fatal("a ref whose LFS files the hub lacks was pushed")
	}
	if !strings.Contains(ev.Error+strings.Join(ev.Warnings, " "), "refs/heads/legacy") {
		t.Fatalf("the held-back ref is not reported: %+v", ev)
	}
}

// Restoring the hub from an older snapshot makes edges rebuild their
// copies: commits pushed after the snapshot must not stay fetchable there.
func TestSnapshotRestoreRebuildsEdges(t *testing.T) {
	e := newEnv(t)
	var edgeTok, dev client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "edge", "scopes": []string{"edge"}}, &edgeTok)
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	hub := e.hubURL(dev.Token, "acme/app")
	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	base := e.commit(work, "a.txt", "1\n")
	e.run(work, "push", "-q", hub, "main")
	n := startEdgeNode(t, e, edgeTok.Token, filepath.Join(t.TempDir(), "edge"), "", "")
	eventually(t, "edge copy", 15*time.Second, func() bool {
		refs, err := lsRefs(n.url(dev.Token, "acme/app"))
		return err == nil && refs["refs/heads/main"] == base
	})

	g := &gitexec.Git{Bin: "git", Env: clientEnv()}
	snap := filepath.Join(t.TempDir(), "snap")
	if _, err := snapshot.Create(t.Context(), e.cfg, g, snap, "test", nil); err != nil {
		t.Fatal(err)
	}
	later := e.commit(work, "secret.txt", "AWS_SECRET=x\n")
	e.run(work, "push", "-q", hub, "main")
	eventually(t, "later commit on the edge", 10*time.Second, func() bool { return fetchBySHA(n.url(dev.Token, "acme/app"), later) == nil })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	e.srv.Shutdown(ctx)
	cancel()
	cfg2 := *e.cfg
	cfg2.DataDir = filepath.Join(t.TempDir(), "restored")
	if _, err := snapshot.Restore(t.Context(), &cfg2, g, snap, nil); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", e.hubAddr)
	if err != nil {
		t.Fatal(err)
	}
	cfg2.PublicURL = "http://" + e.hubAddr
	srv2, err := server.New(&cfg2, slog.New(slog.NewTextHandler(e.logs, nil)), server.Options{NoBackground: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv2.Start(ln); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv2.Shutdown(context.Background()) })

	eventually(t, "edge follows the restored hub", 30*time.Second, func() bool {
		refs, err := lsRefs(n.url(dev.Token, "acme/app"))
		return err == nil && refs["refs/heads/main"] == base
	})
	if fetchBySHA(e.hubURL(dev.Token, "acme/app"), later) == nil {
		t.Fatal("setup: the restored hub has the later commit")
	}
	if fetchBySHA(n.url(dev.Token, "acme/app"), later) == nil {
		t.Fatalf("the edge still serves %s, which the restored hub does not have", later[:12])
	}
}

// Creating a repository by pushing to a new URL is audited like POST
// /api/repos (it gets mirrors from the bindings); release changes do not
// wake edge nodes.
func TestPushCreateAuditAndQuietReleases(t *testing.T) {
	e := newEnv(t)
	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "mirror-a", "provider": "generic", "url_template": e.remoteURL + "/a/{name}.git",
		"credential": map[string]any{"kind": "basic", "username": userA, "secret": passA},
	}, nil)
	e.do(http.MethodPost, "/api/bindings", map[string]any{"org": "acme", "target": "mirror-a"}, nil)
	var dev, edgeTok client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "edge", "scopes": []string{"edge"}}, &edgeTok)
	req, _ := http.NewRequest(http.MethodGet, "http://"+e.hubAddr+"/acme/ghost.git/info/refs?service=git-receive-pack", nil)
	req.SetBasicAuth("x", dev.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var page auditPage
	e.do(http.MethodGet, "/api/audit?repo=acme/ghost", nil, &page)
	row := page.find(func(r auditRow) bool { return r.Action == "create repository (push)" })
	if row == nil || row.Actor != "dev" || row.Via != "http" || row.ClientAddr == "" || !strings.Contains(row.Detail, "mirror-a") {
		t.Fatalf("push-to-create audit entry: %+v (all: %+v)", row, page.Entries)
	}

	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	e.commit(work, "a.txt", "1\n")
	e.run(work, "tag", "v1")
	e.run(work, "push", "-q", e.hubURL(dev.Token, "acme/ghost"), "main", "v1")
	state := func() string {
		req, _ := http.NewRequest(http.MethodGet, "http://"+e.hubAddr+"/api/edge/state?wait=0", nil)
		req.Header.Set("Authorization", "Bearer "+edgeTok.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var st struct {
			Version string `json:"version"`
		}
		json.NewDecoder(resp.Body).Decode(&st)
		return st.Version
	}
	v := state()
	e.do(http.MethodPost, "/api/repos/acme/ghost/releases", map[string]any{"tag": "v1", "name": "One"}, nil)
	e.do(http.MethodPost, "/api/repos/acme/ghost/sync", map[string]any{"wait": true}, nil)
	if v2 := state(); v2 != v {
		t.Fatalf("a release change woke every edge: state %s -> %s", v, v2)
	}
	e.do(http.MethodPatch, "/api/repos/acme/ghost", map[string]any{"visibility": "public"}, nil)
	if v3 := state(); v3 == v {
		t.Fatal("a visibility change did not reach the edges")
	}
}

// A mirror pointed at another location (its URL, or its target's URL
// template or namespace) is treated as a new mirror: what Gitrepo knew
// about the old repository is forgotten, so a new location that already
// has content needs adopting instead of being handled as Gitrepo's own.
func TestMovedMirrorNeedsAdoption(t *testing.T) {
	e := newEnv(t)
	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "mirror-a", "provider": "generic", "url_template": e.remoteURL + "/a/{name}.git",
		"credential": map[string]any{"kind": "basic", "username": userA, "secret": passA},
	}, nil)
	e.do(http.MethodPost, "/api/bindings", map[string]any{"org": "acme", "target": "mirror-a"}, nil)
	var dev client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	for _, p := range []string{"a/other.git", "c/other.git"} {
		if err := e.git.InitBare(t.Context(), e.remoteRepo(p), "main"); err != nil {
			t.Fatal(err)
		}
	}
	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	e.commit(work, "a.txt", "1\n")
	mustContain(t, e.run(work, "push", e.hubURL(dev.Token, "acme/demo"), "main"), "[ok]       mirror-a:demo")
	mustContain(t, e.run(work, "push", e.hubURL(dev.Token, "acme/other"), "main"), "[ok]       mirror-a:other")

	// Someone else's projects at the new locations.
	foreign := filepath.Join(t.TempDir(), "foreign")
	e.run("", "init", "-q", "-b", "main", foreign)
	theirs := e.commit(foreign, "theirs.txt", "not yours\n")
	e.run(foreign, "push", "-q", e.remoteRepo("b/demo.git"), "main")
	e.run(foreign, "push", "-q", e.remoteRepo("c/other.git"), "main")

	// 1. The mirror's URL is changed.
	rem := e.remoteByTarget("acme/demo", "mirror-a")
	e.do(http.MethodPatch, "/api/repos/acme/demo/remotes/"+strconv.FormatInt(rem.ID, 10), map[string]any{"url": e.remoteURL + "/b/demo.git"}, nil)
	ev := eventState(e.sync("acme/demo", false), "mirror-a")
	if !strings.Contains(ev.Error, "adoption required") {
		t.Fatalf("sync to a new URL holding someone else's project: %+v", ev)
	}
	if e.refs(e.remoteRepo("b/demo.git"))["refs/heads/main"] != theirs {
		t.Fatal("the project at the new URL was changed")
	}

	// 2. The target's URL template is changed.
	e.do(http.MethodPatch, "/api/targets/mirror-a", map[string]any{"url_template": e.remoteURL + "/c/{name}.git"}, nil)
	ev = eventState(e.sync("acme/other", false), "mirror-a")
	if !strings.Contains(ev.Error, "adoption required") {
		t.Fatalf("sync after the target moved to a location holding someone else's project: %+v", ev)
	}
	if e.refs(e.remoteRepo("c/other.git"))["refs/heads/main"] != theirs {
		t.Fatal("the project at the target's new location was changed")
	}
	var page auditPage
	e.do(http.MethodGet, "/api/audit?action=targets", nil, &page)
	if page.find(func(r auditRow) bool { return strings.Contains(r.Detail, "1 mirror(s) now point at another location") }) == nil {
		t.Fatalf("the target change does not say mirrors moved: %+v", page.Entries)
	}
}
