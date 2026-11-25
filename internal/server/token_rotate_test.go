package server_test

import (
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/client"
)

func TestTokenRotation(t *testing.T) {
	e := newEnv(t)
	var ci client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "ci", "scopes": []string{"create"}, "repos": []string{"acme/*"}}, &ci)
	path := "/api/tokens/" + strconv.FormatInt(ci.ID, 10)
	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	e.commit(work, "a.txt", "1\n")
	push := func(tok string) error {
		_, err := gitCmd(work, "push", "-q", e.hubURL(tok, "acme/app"), "main")
		return err
	}
	works := func(tok string) bool {
		_, err := gitCmd("", "ls-remote", e.hubURL(tok, "acme/app"))
		return err == nil
	}
	if err := push(ci.Token); err != nil {
		t.Fatal(err)
	}
	find := func() client.Token {
		t.Helper()
		var list []client.Token
		e.do(http.MethodGet, "/api/tokens", nil, &list)
		for _, x := range list {
			if x.ID == ci.ID {
				return x
			}
		}
		t.Fatal("token gone")
		return client.Token{}
	}

	for _, bad := range []string{"abc", "-1h", "31d"} {
		if err := e.api.Do(t.Context(), http.MethodPost, path+"/rotate", map[string]any{"grace": bad}, nil); err == nil {
			t.Fatalf("grace %q accepted", bad)
		}
	}

	// With a grace period both values work; name, scopes and repositories
	// stay; the list shows until when, and whether the old one is used.
	var b client.Token
	e.do(http.MethodPost, path+"/rotate", map[string]any{"grace": "24h"}, &b)
	if b.Token == "" || b.Token == ci.Token || b.Name != "ci" || strings.Join(b.Scopes, ",") != "create" || strings.Join(b.Repos, ",") != "acme/*" {
		t.Fatalf("rotated: %+v", b)
	}
	if b.PreviousValidUntil == nil || time.Until(*b.PreviousValidUntil) < 23*time.Hour || b.RotatedAt == nil {
		t.Fatalf("grace: %+v", b)
	}
	if !works(ci.Token) || !works(b.Token) {
		t.Fatal("both values must work during the grace period")
	}
	if l := find(); l.PreviousLastUsedAt == nil || l.LastUsedAt == nil {
		t.Fatalf("use of each value is not tracked: %+v", l)
	}
	if err := push(b.Token); err != nil {
		t.Fatalf("push with the new value: %v", err)
	}

	// Rotating again ends the earlier grace period.
	var c client.Token
	e.do(http.MethodPost, path+"/rotate", map[string]any{"grace": "1h"}, &c)
	if works(ci.Token) || !works(b.Token) || !works(c.Token) {
		t.Fatal("after a second rotation only the last two values may work")
	}
	// Ending the grace period stops the old value now.
	e.do(http.MethodDelete, path+"/previous", nil, nil)
	if works(b.Token) || !works(c.Token) || find().PreviousValidUntil != nil {
		t.Fatal("end grace")
	}
	// Without a grace period (a leak) the old value stops at once.
	var d client.Token
	e.do(http.MethodPost, path+"/rotate", nil, &d)
	if d.PreviousValidUntil != nil || works(c.Token) || !works(d.Token) {
		t.Fatalf("immediate rotation: %+v", d)
	}

	// Rotations are audited, without the values.
	var audit struct {
		Entries []struct {
			Detail string `json:"detail"`
		} `json:"entries"`
	}
	e.do(http.MethodGet, "/api/audit?action=rotate", nil, &audit)
	rotations := 0
	for _, a := range audit.Entries {
		if strings.Contains(a.Detail, "grp_") {
			t.Fatal("a token value in the audit log")
		}
		if strings.HasPrefix(a.Detail, "token ci rotated") {
			rotations++
		}
	}
	if rotations != 3 { // the refused attempts are logged as errors
		t.Fatalf("audit: %+v", audit.Entries)
	}
}
