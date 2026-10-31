package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/config"
	"github.com/qwq-inc/gitrepo/internal/notify"
)

type receiver struct {
	mu     sync.Mutex
	events []notify.Event
	badSig int
}

func (rc *receiver) wait(t *testing.T, event string, match func(notify.Event) bool) notify.Event {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		rc.mu.Lock()
		for _, e := range rc.events {
			if e.Event == event && (match == nil || match(e)) {
				rc.mu.Unlock()
				return e
			}
		}
		rc.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	t.Fatalf("no %s event; got %+v", event, rc.events)
	return notify.Event{}
}

func (rc *receiver) count(event string) int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	n := 0
	for _, e := range rc.events {
		if e.Event == event {
			n++
		}
	}
	return n
}

func TestWebhooks(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Alerts.FailureThreshold = 2 })
	const slug = "acme/hooks"
	const secret = "hook-secret"

	rc := &receiver{}
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		defer rc.mu.Unlock()
		if r.Header.Get("X-Gitrepo-Signature-256") != notify.Sign([]byte(secret), body) {
			rc.badSig++
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		var ev notify.Event
		json.Unmarshal(body, &ev)
		if ev.Event != r.Header.Get("X-Gitrepo-Event") {
			rc.badSig++
		}
		rc.events = append(rc.events, ev)
	}))
	t.Cleanup(recv.Close)

	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "mirror", "provider": "generic", "url_template": e.remoteURL + "/b/{name}.git",
		"credential": map[string]any{"kind": "token", "secret": tokenB},
	}, nil)
	e.do(http.MethodPost, "/api/bindings", map[string]any{"org": "acme", "target": "mirror"}, nil)
	var hook struct {
		ID int64 `json:"id"`
	}
	e.do(http.MethodPost, "/api/webhooks", map[string]any{"name": "ci", "url": recv.URL + "/hook?key=abc", "secret": secret, "repos": []string{"acme/*"}}, &hook)
	var dev struct {
		Token string `json:"token"`
	}
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	e.git.InitBare(t.Context(), e.remoteRepo("b/hooks.git"), "main")

	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	tip := e.commit(work, "a.txt", "a\n")
	hub := e.hubURL(dev.Token, slug)

	t.Run("test delivery", func(t *testing.T) {
		var res struct {
			OK bool `json:"ok"`
		}
		e.do(http.MethodPost, "/api/webhooks/"+itoa(hook.ID)+"/test", nil, &res)
		if !res.OK {
			t.Fatal("test delivery failed")
		}
		rc.wait(t, notify.EventPing, nil)
	})

	t.Run("push and sync.completed carry the new SHAs", func(t *testing.T) {
		e.run(work, "push", hub, "main")
		p := rc.wait(t, notify.EventPush, nil)
		if p.Repo != slug || p.Refs["refs/heads/main"] != tip || p.Pusher != "dev" {
			t.Fatalf("push event: %+v", p)
		}
		sc := rc.wait(t, notify.EventSyncCompleted, nil)
		if sc.Refs["refs/heads/main"] != tip || len(sc.Remotes) != 1 || sc.Remotes[0].State != "synced" || !strings.HasSuffix(sc.CloneURL, "/acme/hooks.git") {
			t.Fatalf("sync.completed event: %+v", sc)
		}
	})

	t.Run("a no-op sync emits nothing", func(t *testing.T) {
		before := rc.count(notify.EventSyncCompleted)
		e.sync(slug, false)
		time.Sleep(200 * time.Millisecond)
		if rc.count(notify.EventSyncCompleted) != before {
			t.Fatal("sync.completed emitted although nothing changed")
		}
	})

	t.Run("divergence alerts once", func(t *testing.T) {
		rogue := filepath.Join(t.TempDir(), "rogue")
		e.run("", "clone", "-q", e.remoteAuthURL("b/hooks.git"), rogue)
		e.commit(rogue, "r.txt", "r\n")
		e.run(rogue, "push", "-q", "origin", "main")
		e.commit(work, "b.txt", "b\n")
		e.run(work, "push", hub, "main")
		d := rc.wait(t, notify.EventRemoteDiverged, nil)
		if d.Remote != "mirror:hooks" || len(d.Divergent) != 1 || d.Divergent[0] != "refs/heads/main" {
			t.Fatalf("diverged event: %+v", d)
		}
		e.sync(slug, false) // still diverged: no second alert
		time.Sleep(200 * time.Millisecond)
		if n := rc.count(notify.EventRemoteDiverged); n != 1 {
			t.Fatalf("%d divergence alerts", n)
		}
		e.sync(slug, true)
		rc.wait(t, notify.EventRemoteRecovered, nil)
	})

	t.Run("failures alert at the threshold and recover", func(t *testing.T) {
		rem := e.remoteByTarget(slug, "mirror")
		path := "/api/repos/" + slug + "/remotes/" + itoa(rem.ID)
		e.do(http.MethodPatch, path, map[string]any{"url": e.remoteURL + "/b/missing.git"}, nil)
		for i := 0; i < 3 && rc.count(notify.EventRemoteFailed) == 0; i++ {
			e.sync(slug, false)
		}
		f := rc.wait(t, notify.EventRemoteFailed, nil)
		if f.Failures != 2 || !strings.Contains(f.Error, "ls-remote") {
			t.Fatalf("failed event: %+v", f)
		}
		recovered := rc.count(notify.EventRemoteRecovered)
		e.do(http.MethodPatch, path, map[string]any{"url": ""}, nil)
		e.sync(slug, false)
		deadline := time.Now().Add(10 * time.Second)
		for rc.count(notify.EventRemoteRecovered) == recovered {
			if time.Now().After(deadline) {
				t.Fatal("no recovery event")
			}
			time.Sleep(50 * time.Millisecond)
		}
	})

	t.Run("events of other repositories are filtered out", func(t *testing.T) {
		e.do(http.MethodPost, "/api/repos", map[string]any{"slug": "other/app", "bindings": false}, nil)
		e.run(work, "push", e.hubURL(dev.Token, "other/app"), "main")
		time.Sleep(300 * time.Millisecond)
		rc.mu.Lock()
		defer rc.mu.Unlock()
		for _, ev := range rc.events {
			if ev.Repo == "other/app" {
				t.Fatalf("received event for a filtered repository: %+v", ev)
			}
		}
		if rc.badSig != 0 {
			t.Fatalf("%d deliveries had a bad signature", rc.badSig)
		}
	})

	t.Run("webhook URL is not exposed", func(t *testing.T) {
		var hooks []map[string]any
		e.do(http.MethodGet, "/api/webhooks", nil, &hooks)
		raw, _ := json.Marshal(hooks)
		if strings.Contains(string(raw), "key=abc") || !strings.Contains(string(raw), `"signed":true`) {
			t.Fatalf("webhook listing: %s", raw)
		}
	})
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
