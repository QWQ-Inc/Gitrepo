package server_test

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/client"
	"github.com/qwq-inc/gitrepo/internal/releases"
)

type relMirror struct {
	Mirror string `json:"mirror"`
	State  string `json:"state"`
	Error  string `json:"error"`
}

type relOut struct {
	Tag       string `json:"tag"`
	Name      string `json:"name"`
	Body      string `json:"body"`
	Published bool   `json:"published"`
	Assets    []struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
		URL    string `json:"url"`
	} `json:"assets"`
	Mirrors []relMirror       `json:"mirrors"`
	Results []releases.Result `json:"results"`
}

func (r relOut) states() map[string]string {
	out := map[string]string{}
	for _, m := range r.Mirrors {
		out[m.Mirror] = m.State
	}
	return out
}

func allSynced(t *testing.T, r relOut) {
	t.Helper()
	if len(r.Mirrors) != 4 {
		t.Fatalf("mirrors: %+v", r.Mirrors)
	}
	for _, m := range r.Mirrors {
		if m.State != "synced" {
			t.Fatalf("%s: %s %s (all: %+v)", m.Mirror, m.State, m.Error, r.Mirrors)
		}
	}
}

// fakeProjects maps each fake platform to the project path used for acme/svc.
var fakeProjects = map[string]string{"gh": "acme/svc", "gl": "acme-group/svc", "gt": "acme/svc", "ge": "acme/svc"}

func setupFourPlatforms(t *testing.T, e *env, fake *fakePlatforms) {
	for _, tg := range []map[string]any{
		{"name": "github", "provider": "github", "namespace": "acme", "base_url": fake.srv.URL + "/gh", "credential": map[string]any{"kind": "token", "secret": ghToken}},
		{"name": "gitlab", "provider": "gitlab", "namespace": "acme-group", "base_url": fake.srv.URL + "/gl", "credential": map[string]any{"kind": "token", "secret": glToken}},
		{"name": "forgejo", "provider": "gitea", "namespace": "acme", "base_url": fake.srv.URL + "/gt", "credential": map[string]any{"kind": "token", "secret": gtToken}},
		{"name": "gitee", "provider": "gitee", "namespace": "acme", "base_url": fake.srv.URL + "/ge", "credential": map[string]any{"kind": "token", "secret": geToken}},
	} {
		e.do(http.MethodPost, "/api/targets", tg, nil)
		e.do(http.MethodPost, "/api/bindings", map[string]any{"org": "*", "target": tg["name"]}, nil)
	}
}

func TestReleaseSync(t *testing.T) {
	e := newEnv(t)
	fake := newFakePlatforms(t)
	setupFourPlatforms(t, e, fake)
	base := "http://" + e.hubAddr
	var dev, ci, reader client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "ci", "scopes": []string{"write"}, "repos": []string{"acme/*"}}, &ci)
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "reader", "scopes": []string{"read"}}, &reader)
	ciAPI := client.New(base, ci.Token)
	const rel = "/api/repos/acme/svc/releases"

	hub := e.hubURL(dev.Token, "acme/svc")
	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	commit := e.commit(work, "main.go", "package main\n")
	e.run(work, "tag", "-a", "-m", "v1.0", "v1.0")
	mustContain(t, e.run(work, "push", hub, "main", "v1.0"), "[ok]       github:acme/svc", "[ok]       gitee:acme/svc")

	put := func(tok, tag, name string, content []byte) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPut, base+rel+"/"+tag+"/assets/"+name, bytes.NewReader(content))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	mirrored := func(tag string) map[string]map[string]string {
		out := map[string]map[string]string{}
		for plat, proj := range fakeProjects {
			r := fake.release(plat, proj, tag)
			if r == nil {
				continue
			}
			files := map[string]string{"#name": r.Name, "#body": r.Body}
			for _, a := range r.Assets {
				files[a.Name] = a.SHA
			}
			out[plat] = files
		}
		return out
	}
	tarball := []byte("pretend this is a tarball\n")
	sums := []byte(sha256hex(tarball) + "  app.tar.gz\n")

	t.Run("a write token creates a draft, attaches files and publishes to every platform", func(t *testing.T) {
		var r relOut
		if err := ciAPI.Do(t.Context(), http.MethodPost, rel, map[string]any{"tag": "v1.0", "name": "Version 1.0", "body": "First release."}, &r); err != nil {
			t.Fatal(err)
		}
		if r.Published || r.states()["github:acme/svc"] != "draft" {
			t.Fatalf("draft: %+v", r)
		}
		if resp := put(ci.Token, "v1.0", "app.tar.gz", tarball); resp.StatusCode != 201 {
			t.Fatalf("upload: %s", resp.Status)
		}
		put(ci.Token, "v1.0", "SHA256SUMS", sums)
		if len(mirrored("v1.0")) != 0 {
			t.Fatal("a draft reached a mirror")
		}
		if err := ciAPI.Do(t.Context(), http.MethodPost, rel+"/v1.0/publish", map[string]any{"wait": true}, &r); err != nil {
			t.Fatal(err)
		}
		allSynced(t, r)
		got := mirrored("v1.0")
		for _, plat := range []string{"gh", "gl", "gt", "ge"} {
			m := got[plat]
			if m == nil || m["#name"] != "Version 1.0" || m["#body"] != "First release." || m["app.tar.gz"] != sha256hex(tarball) || m["SHA256SUMS"] != sha256hex(sums) {
				t.Errorf("%s: %+v", plat, m)
			}
		}
		if g := fake.release("ge", "acme/svc", "v1.0"); g.Target != commit {
			t.Errorf("gitee target_commitish = %q, want %s", g.Target, commit)
		}
	})

	t.Run("editing the notes updates the platforms without re-uploading files", func(t *testing.T) {
		uploads := fake.countLog("gh upload")
		var r relOut
		if err := ciAPI.Do(t.Context(), http.MethodPatch, rel+"/v1.0", map[string]any{"body": "First release, now with notes.", "wait": true}, &r); err != nil {
			t.Fatal(err)
		}
		allSynced(t, r)
		for plat, m := range mirrored("v1.0") {
			if m["#body"] != "First release, now with notes." {
				t.Errorf("%s body: %q", plat, m["#body"])
			}
		}
		if fake.countLog("gh upload") != uploads {
			t.Error("unchanged files were uploaded again")
		}
		// Publishing again with nothing changed costs no API calls.
		updates := fake.countLog("gh update")
		ciAPI.Do(t.Context(), http.MethodPost, rel+"/v1.0/publish", map[string]any{"wait": true}, &r)
		if fake.countLog("gh update") != updates {
			t.Error("an unchanged release was sent again")
		}
	})

	t.Run("replacing and removing files is mirrored", func(t *testing.T) {
		tarball2 := []byte("a rebuilt tarball\n")
		put(ci.Token, "v1.0", "app.tar.gz", tarball2)
		req, _ := http.NewRequest(http.MethodDelete, base+rel+"/v1.0/assets/SHA256SUMS", nil)
		req.Header.Set("Authorization", "Bearer "+ci.Token)
		if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 204 {
			t.Fatalf("delete asset: %v %v", err, resp)
		}
		var r relOut
		if err := ciAPI.Do(t.Context(), http.MethodPost, rel+"/v1.0/publish", map[string]any{"wait": true}, &r); err != nil {
			t.Fatal(err)
		}
		allSynced(t, r)
		for plat, m := range mirrored("v1.0") {
			if m["app.tar.gz"] != sha256hex(tarball2) || m["SHA256SUMS"] != "" || len(m) != 3 {
				t.Errorf("%s: %+v", plat, m)
			}
		}
	})

	t.Run("files can be downloaded from the hub with read access", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, base+rel+"/v1.0/assets/app.tar.gz", nil)
		req.Header.Set("Authorization", "Bearer "+reader.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != "a rebuilt tarball\n" || !strings.Contains(resp.Header.Get("Content-Disposition"), "app.tar.gz") {
			t.Fatalf("download: %s %q %v", resp.Status, body, resp.Header)
		}
		if resp := put(reader.Token, "v1.0", "evil.bin", []byte("x")); resp.StatusCode != 403 {
			t.Fatalf("a read-only token uploaded a file: %s", resp.Status)
		}
		resp, _ = http.Get(base + rel)
		if resp.StatusCode != 401 {
			t.Fatalf("anonymous access to a private repository's releases: %s", resp.Status)
		}
		if resp := put(ci.Token, "v1.0", `a%5Cb`, []byte("x")); resp.StatusCode != 400 {
			t.Fatalf("invalid file name accepted: %s", resp.Status)
		}
	})

	t.Run("a release waits until its tag has reached the mirror", func(t *testing.T) {
		// Tag the hub directly: no push, so no sync yet.
		e.run(e.hubRepo("acme/svc"), "tag", "v2.0", "main")
		var r relOut
		if err := ciAPI.Do(t.Context(), http.MethodPost, rel, map[string]any{"tag": "v2.0", "publish": true, "wait": true}, &r); err != nil {
			t.Fatal(err)
		}
		for _, m := range r.Mirrors {
			if m.State != "waiting" {
				t.Fatalf("%s: %s, want waiting", m.Mirror, m.State)
			}
		}
		if len(mirrored("v2.0")) != 0 {
			t.Fatal("a release was created before its tag reached the mirror")
		}
		e.sync("acme/svc", false) // the tag reaches the mirrors; the release follows
		deadline := time.Now().Add(20 * time.Second)
		for {
			e.do(http.MethodGet, rel+"/v2.0", nil, &r)
			if done := r.states(); done["github:acme/svc"] == "synced" && done["gitlab:acme-group/svc"] == "synced" && done["forgejo:acme/svc"] == "synced" && done["gitee:acme/svc"] == "synced" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("release not published after the sync: %+v", r.Mirrors)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if len(mirrored("v2.0")) != 4 {
			t.Fatalf("mirrored: %v", mirrored("v2.0"))
		}
	})

	t.Run("a release made by hand on a platform is not taken over without adopt", func(t *testing.T) {
		e.run(work, "tag", "v3.0")
		e.run(work, "push", "-q", hub, "v3.0")
		fake.addForeignRelease("gh", "acme/svc", "v3.0", "manual.zip")
		var r relOut
		if err := ciAPI.Do(t.Context(), http.MethodPost, rel, map[string]any{"tag": "v3.0", "body": "three", "publish": true, "wait": true}, &r); err != nil {
			t.Fatal(err)
		}
		st := r.states()
		if st["github:acme/svc"] != "conflict" || st["gitlab:acme-group/svc"] != "synced" {
			t.Fatalf("states: %+v", r.Mirrors)
		}
		if g := fake.release("gh", "acme/svc", "v3.0"); g.Body != "by hand" {
			t.Fatal("the hand-made release was changed")
		}
		put(ci.Token, "v3.0", "app.tar.gz", tarball)
		if err := ciAPI.Do(t.Context(), http.MethodPost, rel+"/v3.0/publish", map[string]any{"wait": true, "adopt": true}, &r); err != nil {
			t.Fatal(err)
		}
		allSynced(t, r)
		g := mirrored("v3.0")["gh"]
		if g["#body"] != "three" || g["app.tar.gz"] != sha256hex(tarball) || g["manual.zip"] != "foreign" {
			t.Fatalf("adopted release: %+v (its own files must stay)", g)
		}
	})

	t.Run("deleting a release removes it from the platforms and the hub", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete, base+rel+"/v1.0", nil)
		req.Header.Set("Authorization", "Bearer "+ci.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("delete: %v %v", err, resp)
		}
		resp.Body.Close()
		if m := mirrored("v1.0"); len(m) != 0 {
			t.Fatalf("still on mirrors: %v", m)
		}
		var list []relOut
		e.do(http.MethodGet, rel, nil, &list)
		for _, r := range list {
			if r.Tag == "v1.0" {
				t.Fatal("still on the hub")
			}
		}
		// The rebuilt tarball was only used by v1.0 and is gone; the original
		// is still attached to v3.0.
		if _, err := os.Stat(releases.BlobPath(e.hubRepo("acme/svc"), sha256hex([]byte("a rebuilt tarball\n")))); err == nil {
			t.Fatal("an unused release file was kept")
		}
		if _, err := os.Stat(releases.BlobPath(e.hubRepo("acme/svc"), sha256hex(tarball))); err != nil {
			t.Fatal("a release file still in use was deleted")
		}
	})

	t.Run("tags with slashes work (escaped as %2F)", func(t *testing.T) {
		e.run(work, "tag", "lib/v1.2")
		e.run(work, "push", "-q", hub, "lib/v1.2")
		var r relOut
		if err := ciAPI.Do(t.Context(), http.MethodPost, rel, map[string]any{"tag": "lib/v1.2", "publish": true, "wait": true}, &r); err != nil {
			t.Fatal(err)
		}
		allSynced(t, r)
		if resp := put(ci.Token, "lib%2Fv1.2", "lib.zip", []byte("zip")); resp.StatusCode != 201 {
			t.Fatalf("upload to a slashed tag: %s", resp.Status)
		}
		if err := ciAPI.Do(t.Context(), http.MethodPost, rel+"/lib%2Fv1.2/publish", map[string]any{"wait": true}, &r); err != nil {
			t.Fatal(err)
		}
		allSynced(t, r)
		if m := mirrored("lib/v1.2"); len(m) != 4 || m["gh"]["lib.zip"] != sha256hex([]byte("zip")) || m["gl"]["lib.zip"] == "" {
			t.Fatalf("mirrored: %v", m)
		}
	})

	t.Run("release changes are audited with the token that made them", func(t *testing.T) {
		var page auditPage
		e.do(http.MethodGet, "/api/audit?actor=ci&action=releases", nil, &page)
		if len(page.Entries) == 0 || page.find(func(r auditRow) bool { return r.Detail == "release v1.0 (published false)" && r.Repo == "acme/svc" }) == nil {
			t.Fatalf("audit: %+v", page.Entries)
		}
	})
}
