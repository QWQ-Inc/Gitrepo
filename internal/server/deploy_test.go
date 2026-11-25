package server_test

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/client"
	"github.com/qwq-inc/gitrepo/internal/deploy"
)

// deployReceiver records deploy hook deliveries per path.
type deployReceiver struct {
	srv  *httptest.Server
	mu   sync.Mutex
	got  map[string][]delivery
	fail map[string]int // path → deliveries left to refuse
}

type delivery struct {
	header  http.Header
	body    []byte
	payload deploy.Payload
}

func newDeployReceiver(t *testing.T) *deployReceiver {
	d := &deployReceiver{got: map[string][]delivery{}, fail: map[string]int{}}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p deploy.Payload
		json.Unmarshal(body, &p)
		d.mu.Lock()
		defer d.mu.Unlock()
		d.got[r.URL.Path] = append(d.got[r.URL.Path], delivery{header: r.Header.Clone(), body: body, payload: p})
		if d.fail[r.URL.Path] > 0 {
			d.fail[r.URL.Path]--
			http.Error(w, "deploy service busy", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *deployReceiver) deliveries(path string) []delivery {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]delivery(nil), d.got[path]...)
}

func TestDeployHooks(t *testing.T) {
	e := newEnv(t)
	rcv := newDeployReceiver(t)
	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "mirror-a", "provider": "generic", "url_template": e.remoteURL + "/a/{name}.git",
		"credential": map[string]any{"kind": "basic", "username": userA, "secret": passA},
	}, nil)
	var dev client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	hub := e.hubURL(dev.Token, "acme/demo")
	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	e.commit(work, "README.md", "one\n")
	e.run(work, "push", "-q", hub, "main")
	e.do(http.MethodPost, "/api/repos/acme/demo/remotes", map[string]any{"target": "mirror-a"}, nil)
	e.sync("acme/demo", false)
	mirror := e.remoteByTarget("acme/demo", "mirror-a")

	key := []byte("signing key")
	secret := "whsec_" + base64.StdEncoding.EncodeToString(key)
	add := func(body map[string]any) client.DeployHook {
		t.Helper()
		var h client.DeployHook
		e.do(http.MethodPost, "/api/repos/acme/demo/deploy-hooks", body, &h)
		return h
	}
	hubHook := add(map[string]any{"name": "hub", "url": rcv.srv.URL + "/hub", "secret": secret, "token": "deploy-token"})
	mirHook := add(map[string]any{"name": "se", "kind": "stackenigma", "url": rcv.srv.URL + "/mirror", "source": mirror.Label})
	add(map[string]any{"name": "dev", "url": rcv.srv.URL + "/dev", "branch": "dev"})
	off := add(map[string]any{"name": "off", "url": rcv.srv.URL + "/off"})
	e.do(http.MethodPatch, "/api/repos/acme/demo/deploy-hooks/"+strconv.FormatInt(off.ID, 10), map[string]any{"enabled": false}, nil)
	if hubHook.Branch != "main" || hubHook.Source != "hub" || !hubHook.Signed || !hubHook.Token || mirHook.Source != mirror.Label {
		t.Fatalf("hooks: %+v %+v", hubHook, mirHook)
	}
	for _, bad := range []map[string]any{
		{"name": "x", "url": "ftp://x"},
		{"name": "x", "url": "https://u:p@x.example.com/"},
		{"name": "x", "url": rcv.srv.URL, "source": "nowhere"},
		{"name": "x", "url": rcv.srv.URL, "secret": "whsec_!!"},
		{"name": "x", "url": rcv.srv.URL, "branch": "a..b"},
	} {
		if err := e.api.Do(t.Context(), http.MethodPost, "/api/repos/acme/demo/deploy-hooks", bad, nil); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}

	// A push to main: the hub hook fires at once; the mirror hook once the
	// mirror has the commit; the dev and disabled hooks stay quiet.
	sha := e.commit(work, "README.md", "two\n")
	e.run(work, "push", "-q", hub, "main")
	eventually(t, "both hooks delivered", 15*time.Second, func() bool {
		return len(rcv.deliveries("/hub")) == 1 && len(rcv.deliveries("/mirror")) == 1
	})
	d := rcv.deliveries("/hub")[0]
	if p := d.payload; p.Type != "deploy.requested" || p.Data.Commit != sha || p.Data.Branch != "main" || p.Data.Source != "hub" ||
		p.Data.Trigger != "push" || p.Data.Repository != "acme/demo" || p.Data.Pusher == "" {
		t.Fatalf("hub delivery: %+v", p)
	}
	ts, _ := strconv.ParseInt(d.header.Get("webhook-timestamp"), 10, 64)
	if !deploy.Verify(key, d.header.Get("webhook-id"), ts, d.body, d.header.Get("webhook-signature")) {
		t.Fatalf("bad Standard Webhooks signature: %v", d.header)
	}
	if d.header.Get("Authorization") != "Bearer deploy-token" || !strings.HasPrefix(d.header.Get("X-Gitrepo-Signature-256"), "sha256=") {
		t.Fatalf("headers: %v", d.header)
	}
	m := rcv.deliveries("/mirror")[0]
	if p := m.payload; p.Data.Commit != sha || p.Data.Source != mirror.Label || p.Data.Trigger != "sync" || m.header.Get("webhook-signature") != "" {
		t.Fatalf("mirror delivery: %+v %v", p, m.header)
	}
	if got := e.refs(e.remoteRepo("a/demo.git"))["refs/heads/main"]; got != sha {
		t.Fatalf("the mirror hook fired before the mirror had %s (it has %s)", sha, got)
	}
	if len(rcv.deliveries("/dev")) != 0 || len(rcv.deliveries("/off")) != 0 {
		t.Fatal("a hook for another branch, or a disabled one, fired")
	}

	// A push to dev fires only the dev hook.
	e.run(work, "checkout", "-q", "-b", "dev")
	devSHA := e.commit(work, "dev.txt", "dev\n")
	e.run(work, "push", "-q", hub, "dev")
	eventually(t, "dev hook", 15*time.Second, func() bool { return len(rcv.deliveries("/dev")) == 1 })
	if rcv.deliveries("/dev")[0].payload.Data.Commit != devSHA || len(rcv.deliveries("/hub")) != 1 {
		t.Fatal("dev push")
	}

	// A failing platform is retried, and the outcome is shown.
	rcv.mu.Lock()
	rcv.fail["/hub"] = 1
	rcv.mu.Unlock()
	e.run(work, "checkout", "-q", "main")
	sha3 := e.commit(work, "README.md", "three\n")
	e.run(work, "push", "-q", hub, "main")
	eventually(t, "retried delivery", 15*time.Second, func() bool { return len(rcv.deliveries("/hub")) == 3 })
	var hooks []client.DeployHook
	eventually(t, "delivery recorded", 5*time.Second, func() bool {
		e.do(http.MethodGet, "/api/repos/acme/demo/deploy-hooks", nil, &hooks)
		for _, h := range hooks {
			if h.Name == "hub" {
				return h.LastCommit == sha3 && strings.HasPrefix(h.LastStatus, "202")
			}
		}
		return false
	})

	// A manual trigger deploys the current commit again.
	var tr client.DeployTrigger
	e.do(http.MethodPost, "/api/repos/acme/demo/deploy-hooks/"+strconv.FormatInt(mirHook.ID, 10)+"/trigger", nil, &tr)
	if !tr.OK || tr.Commit != sha3 {
		t.Fatalf("trigger: %+v", tr)
	}
	last := rcv.deliveries("/mirror")
	if p := last[len(last)-1].payload; p.Data.Trigger != "manual" || p.Data.Commit != sha3 {
		t.Fatalf("manual delivery: %+v", p)
	}
	// A refused manual trigger says why.
	rcv.mu.Lock()
	rcv.fail["/mirror"] = 1
	rcv.mu.Unlock()
	e.do(http.MethodPost, "/api/repos/acme/demo/deploy-hooks/"+strconv.FormatInt(mirHook.ID, 10)+"/trigger", nil, &tr)
	if tr.OK || !strings.Contains(tr.Status, "503") || !strings.Contains(tr.Status, "deploy service busy") {
		t.Fatalf("failed trigger: %+v", tr)
	}

	// Removing the mirror removes the hooks that wait for it.
	e.do(http.MethodDelete, "/api/repos/acme/demo/remotes/"+strconv.FormatInt(mirror.ID, 10), nil, nil)
	e.do(http.MethodGet, "/api/repos/acme/demo/deploy-hooks", nil, &hooks)
	for _, h := range hooks {
		if h.Name == "se" {
			t.Fatal("a deploy hook outlived its mirror")
		}
	}
}

func TestDeploySignature(t *testing.T) {
	key, err := deploy.SecretKey("whsec_" + base64.StdEncoding.EncodeToString([]byte("k")))
	if err != nil || string(key) != "k" {
		t.Fatalf("%q %v", key, err)
	}
	// The example from the Standard Webhooks specification.
	key, _ = deploy.SecretKey("whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw")
	body := []byte(`{"test": 2432232314}`)
	want := "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="
	if got := deploy.Sign(key, "msg_p5jXN8AQM9LWM0D4loKWxJek", 1614265330, body); got != want {
		t.Fatalf("signature %s, want %s", got, want)
	}
	if !deploy.Verify(key, "msg_p5jXN8AQM9LWM0D4loKWxJek", 1614265330, body, "v1,other "+want) {
		t.Fatal("verify")
	}
}
