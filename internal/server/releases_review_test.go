package server_test

import (
	"bytes"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/client"
)

// Regression tests for the review of release publishing.
func TestReleaseOwnershipAndFailures(t *testing.T) {
	e := newEnv(t)
	fake := newFakePlatforms(t)
	setupFourPlatforms(t, e, fake)
	base := "http://" + e.hubAddr
	var dev, ci, reader client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "ci", "scopes": []string{"write"}}, &ci)
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "reader", "scopes": []string{"read"}}, &reader)
	ciAPI := client.New(base, ci.Token)
	const rel = "/api/repos/acme/svc/releases"
	hub := e.hubURL(dev.Token, "acme/svc")
	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	e.commit(work, "main.go", "package main\n")
	tag := func(name string) {
		e.run(work, "tag", name)
		e.run(work, "push", "-q", hub, "main", name)
	}
	put := func(tg, name string, content []byte) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPut, base+rel+"/"+tg+"/assets/"+strings.ReplaceAll(name, " ", "%20"), bytes.NewReader(content))
		req.Header.Set("Authorization", "Bearer "+ci.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 201 {
			t.Fatalf("upload %s: %v %v", name, err, resp)
		}
		resp.Body.Close()
	}
	publish := func(tg string, body map[string]any) relOut {
		t.Helper()
		var r relOut
		if body == nil {
			body = map[string]any{}
		}
		body["wait"] = true
		if err := ciAPI.Do(t.Context(), http.MethodPost, rel+"/"+tg+"/publish", body, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	del := func(tg, query string) (int, string) {
		req, _ := http.NewRequest(http.MethodDelete, base+rel+"/"+tg+query, nil)
		req.Header.Set("Authorization", "Bearer "+ci.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}

	t.Run("files GitHub renames are tracked by ID and do not break later updates", func(t *testing.T) {
		tag("r1")
		e.do(http.MethodPost, rel, map[string]any{"tag": "r1"}, nil)
		put("r1", "my file.txt", []byte("one"))
		allSynced(t, publish("r1", nil))
		gh := fake.release("gh", "acme/svc", "r1")
		if len(gh.Assets) != 1 || gh.Assets[0].Name != "my.file.txt" {
			t.Fatalf("github assets: %+v", gh.Assets)
		}
		uploads := fake.countLog("gh upload")
		var r relOut
		if err := ciAPI.Do(t.Context(), http.MethodPatch, rel+"/r1", map[string]any{"body": "edited", "wait": true}, &r); err != nil {
			t.Fatal(err)
		}
		allSynced(t, r)
		allSynced(t, publish("r1", map[string]any{"force": true}))
		if fake.countLog("gh upload") != uploads {
			t.Fatal("a renamed file was uploaded again")
		}
		req, _ := http.NewRequest(http.MethodDelete, base+rel+"/r1/assets/my%20file.txt", nil)
		req.Header.Set("Authorization", "Bearer "+ci.Token)
		http.DefaultClient.Do(req)
		allSynced(t, publish("r1", nil))
		if gh := fake.release("gh", "acme/svc", "r1"); len(gh.Assets) != 0 {
			t.Fatalf("the renamed file was not removed on GitHub: %+v", gh.Assets)
		}
	})

	t.Run("a create whose answer was lost is claimed on retry, not a conflict", func(t *testing.T) {
		tag("r2")
		e.do(http.MethodPost, rel, map[string]any{"tag": "r2"}, nil)
		fake.inject("lose", "gt", true)
		if st := publish("r2", nil).states()["forgejo:acme/svc"]; st != "failed" {
			t.Fatalf("forgejo after a lost answer: %s", st)
		}
		fake.inject("lose", "gt", false)
		if st := publish("r2", nil).states()["forgejo:acme/svc"]; st != "synced" {
			t.Fatalf("forgejo on retry: %s", st)
		}
	})

	t.Run("a GitLab release made by hand for the same tag is not treated as ours", func(t *testing.T) {
		tag("r3")
		e.do(http.MethodPost, rel, map[string]any{"tag": "r3", "body": "ours"}, nil)
		allSynced(t, publish("r3", nil))
		fake.mu.Lock()
		delete(fake.relStore().rels, "gl|acme-group/svc|r3")
		fake.mu.Unlock()
		fake.addForeignRelease("gl", "acme-group/svc", "r3")
		r := publish("r3", map[string]any{"force": true})
		if st := r.states()["gitlab:acme-group/svc"]; st != "conflict" {
			t.Fatalf("gitlab: %s", st)
		}
		if gl := fake.release("gl", "acme-group/svc", "r3"); gl.Body != "by hand" {
			t.Fatal("the hand-made GitLab release was overwritten")
		}
		if code, body := del("r3", ""); code != 200 {
			t.Fatalf("delete: %d %s", code, body)
		}
		if fake.release("gl", "acme-group/svc", "r3") == nil {
			t.Fatal("the hand-made GitLab release was deleted")
		}
		if fake.release("gh", "acme/svc", "r3") != nil {
			t.Fatal("our GitHub release was not deleted")
		}
	})

	t.Run("deleting an adopted release only removes Gitrepo's files", func(t *testing.T) {
		tag("r4")
		fake.addForeignRelease("gt", "acme/svc", "r4", "manual.zip")
		e.do(http.MethodPost, rel, map[string]any{"tag": "r4"}, nil)
		put("r4", "app.bin", []byte("ours"))
		e.do(http.MethodPost, rel+"/r4/publish", map[string]any{"wait": true, "adopt": true}, nil)
		if code, body := del("r4", ""); code != 200 {
			t.Fatalf("delete: %d %s", code, body)
		}
		gt := fake.release("gt", "acme/svc", "r4")
		if gt == nil || len(gt.Assets) != 1 || gt.Assets[0].Name != "manual.zip" {
			t.Fatalf("adopted release after delete: %+v", gt)
		}
	})

	t.Run("a partly failed delete unpublishes instead of re-creating where it was deleted", func(t *testing.T) {
		tag("r5")
		e.do(http.MethodPost, rel, map[string]any{"tag": "r5", "publish": true, "wait": true}, nil)
		fake.inject("delete", "gl", true)
		if code, _ := del("r5", ""); code != http.StatusBadGateway {
			t.Fatalf("partial delete: %d", code)
		}
		if fake.release("gh", "acme/svc", "r5") != nil || fake.release("gl", "acme-group/svc", "r5") == nil {
			t.Fatal("setup")
		}
		e.sync("acme/svc", false) // used to bring it back on GitHub
		time.Sleep(300 * time.Millisecond)
		if fake.release("gh", "acme/svc", "r5") != nil {
			t.Fatal("the release was re-created on GitHub after a partial delete")
		}
		var r relOut
		e.do(http.MethodGet, rel+"/r5", nil, &r)
		if r.Published {
			t.Fatal("still published")
		}
		fake.inject("delete", "gl", false)
		if code, body := del("r5", ""); code != 200 || fake.release("gl", "acme-group/svc", "r5") != nil {
			t.Fatalf("retry: %d %s", code, body)
		}
	})

	t.Run("a failing mirror waits for its backoff in the background", func(t *testing.T) {
		tag("r6")
		fake.inject("create", "gt", true)
		e.do(http.MethodPost, rel, map[string]any{"tag": "r6", "publish": true, "wait": true}, nil)
		before := fake.countLog("gt create-failed r6")
		for i := 0; i < 3; i++ {
			e.sync("acme/svc", false)
		}
		time.Sleep(300 * time.Millisecond)
		if n := fake.countLog("gt create-failed r6"); n != before {
			t.Fatalf("retried %d times in the background before the backoff expired", n-before)
		}
		publish("r6", nil) // a person asking retries now
		if n := fake.countLog("gt create-failed r6"); n != before+1 {
			t.Fatalf("an explicit publish did not retry: %d", n-before)
		}
		fake.inject("create", "gt", false)
	})

	t.Run("drafts and mirror details are hidden from readers", func(t *testing.T) {
		tag("r7")
		e.do(http.MethodPost, rel, map[string]any{"tag": "r7", "body": "embargoed"}, nil)
		put("r7", "fix.patch", []byte("secret fix"))
		readerAPI := client.New(base, reader.Token)
		var list []relOut
		if err := readerAPI.Do(t.Context(), http.MethodGet, rel, nil, &list); err != nil {
			t.Fatal(err)
		}
		for _, r := range list {
			if r.Tag == "r7" {
				t.Fatal("a draft is listed to a reader")
			}
			if len(r.Mirrors) != 0 {
				t.Fatalf("mirror details shown to a reader: %+v", r.Mirrors)
			}
		}
		if err := readerAPI.Do(t.Context(), http.MethodGet, rel+"/r7", nil, nil); err == nil {
			t.Fatal("a reader read a draft")
		}
		req, _ := http.NewRequest(http.MethodGet, base+rel+"/r7/assets/fix.patch", nil)
		req.Header.Set("Authorization", "Bearer "+reader.Token)
		if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 404 {
			t.Fatalf("a reader downloaded a draft's file: %s", resp.Status)
		}
	})

	t.Run("a file shared by two releases survives an upload during a publication and a delete", func(t *testing.T) {
		content := []byte("shared binary")
		tag("v9")
		tag("v9-rc1")
		e.do(http.MethodPost, rel, map[string]any{"tag": "v9"}, nil)
		put("v9", "notes.txt", []byte("notes"))
		e.do(http.MethodPost, rel, map[string]any{"tag": "v9-rc1"}, nil)
		put("v9-rc1", "app.bin", content)
		fake.inject("slow", "gh", true)
		defer fake.inject("slow", "gh", false)
		published := make(chan struct{})
		go func() { defer close(published); publish("v9", nil) }() // holds v9 while uploading slowly
		time.Sleep(300 * time.Millisecond)
		uploaded := make(chan struct{})
		go func() { defer close(uploaded); put("v9", "app.bin", content) }() // same content as the rc
		time.Sleep(200 * time.Millisecond)
		if code, body := del("v9-rc1", "?hub_only=true"); code != 200 {
			t.Fatalf("delete rc: %d %s", code, body)
		}
		<-uploaded
		<-published
		req, _ := http.NewRequest(http.MethodGet, base+rel+"/v9/assets/app.bin", nil)
		req.Header.Set("Authorization", "Bearer "+ci.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !bytes.Equal(got, content) {
			t.Fatalf("v9's file after the rc was deleted: %s %q", resp.Status, got)
		}
	})
}
