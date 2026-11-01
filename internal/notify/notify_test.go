package notify

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMatches(t *testing.T) {
	cases := []struct {
		subs  []string
		event string
		want  bool
	}{
		{nil, EventPush, true},
		{[]string{"*"}, EventRemoteFailed, true},
		{[]string{EventPush}, EventPush, true},
		{[]string{EventPush}, EventSyncCompleted, false},
		{[]string{"remote.*"}, EventRemoteDiverged, true},
		{[]string{"remote.*"}, EventSyncCompleted, false},
	}
	for _, c := range cases {
		if got := Matches(c.subs, c.event); got != c.want {
			t.Errorf("Matches(%v, %s) = %v", c.subs, c.event, got)
		}
	}
}

func TestBodies(t *testing.T) {
	e := &Event{Event: EventRemoteDiverged, Repo: "acme/demo", Remote: "github:acme/demo", Divergent: []string{"refs/heads/main"}}
	for format, key := range map[string]string{"slack": "text", "wecom": "msgtype", "dingtalk": "msgtype", "feishu": "msg_type", "json": "event"} {
		b, err := Body(format, e)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil || m[key] == nil {
			t.Errorf("%s body %s lacks %q", format, b, key)
		}
		if format != "json" && !strings.Contains(string(b), "gr sync acme/demo --overwrite") {
			t.Errorf("%s body lacks the remedy: %s", format, b)
		}
	}
	if _, err := Body("teams", e); err == nil {
		t.Error("unknown format accepted")
	}
}

func TestSign(t *testing.T) {
	// echo -n 'body' | openssl dgst -sha256 -hmac secret
	if got := Sign([]byte("secret"), []byte("body")); got != "sha256=dc46983557fea127b43af721467eb9b3fde2338fe3e14f51952aa8478c13d355" {
		t.Fatalf("Sign = %s", got)
	}
}

func TestPushText(t *testing.T) {
	e := &Event{Event: EventPush, Repo: "acme/demo", Pusher: "laptop", Refs: map[string]string{"refs/heads/main": "0123456789abcdef", "refs/heads/old": ""}}
	txt := e.Text()
	for _, want := range []string{"acme/demo received a push from laptop", "refs/heads/main → 0123456789", "refs/heads/old deleted"} {
		if !strings.Contains(txt, want) {
			t.Errorf("text %q lacks %q", txt, want)
		}
	}
}

// Instance-wide events (credential.expiring names targets) reach only
// webhooks that are not limited to some repositories.
func TestInstanceEventsSkipRepoScopedWebhooks(t *testing.T) {
	cases := []struct {
		patterns []string
		repo     string
		want     bool
	}{
		{nil, "", true},
		{[]string{"*"}, "", true},
		{[]string{"*/*"}, "", true},
		{[]string{"team/*"}, "", false},
		{[]string{"team/*", "*"}, "", false},
		{[]string{"team/*"}, "team/app", true},
		{[]string{"team/*"}, "other/app", false},
		{nil, "other/app", true},
	}
	for _, c := range cases {
		if got := wants(c.patterns, c.repo); got != c.want {
			t.Errorf("wants(%v, %q) = %v, want %v", c.patterns, c.repo, got, c.want)
		}
	}
}
