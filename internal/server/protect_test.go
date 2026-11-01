package server_test

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/qwq-inc/gitrepo/internal/client"
	"github.com/qwq-inc/gitrepo/internal/config"
)

// Protected branches cannot be force-pushed or deleted on the hub (over
// HTTPS or SSH), so their history on the mirrors cannot be rewritten
// either; protected tags cannot be moved.
func TestBranchProtection(t *testing.T) {
	_, sshErr := exec.LookPath("ssh")
	e := newEnv(t, func(c *config.Config) {
		if sshErr == nil {
			c.SSH.Listen = "127.0.0.1:0"
		}
	})
	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "mirror-a", "provider": "generic", "url_template": e.remoteURL + "/a/{name}.git",
		"credential": map[string]any{"kind": "basic", "username": userA, "secret": passA},
	}, nil)
	e.do(http.MethodPost, "/api/bindings", map[string]any{"org": "acme", "target": "mirror-a"}, nil)
	var dev client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	hub := e.hubURL(dev.Token, "acme/demo")
	mirror := e.remoteRepo("a/demo.git")

	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	first := e.commit(work, "a.txt", "1\n")
	e.run(work, "branch", "feature")
	e.run(work, "tag", "v1.0")
	mustContain(t, e.run(work, "push", hub, "main", "feature", "v1.0"), "[ok]       mirror-a:demo")

	var rule, tagRule struct {
		ID int64 `json:"id"`
	}
	e.do(http.MethodPost, "/api/protections", map[string]any{"repos": "acme/*", "ref": "main"}, &rule)
	e.do(http.MethodPost, "/api/protections", map[string]any{"repos": "*", "ref": "refs/tags/v*"}, &tagRule)
	if err := e.api.Do(t.Context(), http.MethodPost, "/api/protections", map[string]any{"repos": "acme/*", "ref": "main"}, nil); err == nil {
		t.Fatal("a duplicate rule was accepted")
	}
	if err := e.api.Do(t.Context(), http.MethodPost, "/api/protections", map[string]any{"repos": "*", "ref": "refs/backup/*"}, nil); err == nil {
		t.Fatal("a rule for hidden refs was accepted")
	}
	var st client.Repo
	e.do(http.MethodGet, "/api/repos/acme/demo", nil, &st)
	if len(st.Protected) != 2 {
		t.Fatalf("repository view: protected %+v", st.Protected)
	}

	// A fast-forward is fine.
	second := e.commit(work, "a.txt", "2\n")
	e.run(work, "push", "-q", hub, "main")

	// Rewriting main is refused, and the mirror keeps its history.
	e.run(work, "reset", "-q", "--hard", first)
	rewritten := e.commit(work, "b.txt", "rewritten\n")
	out, err := gitCmd(work, "push", "-f", hub, "main")
	if err == nil {
		t.Fatalf("force-push to a protected branch succeeded:\n%s", out)
	}
	mustContain(t, out, "refs/heads/main: protected on the hub; force-pushes")
	if e.refs(e.hubRepo("acme/demo"))["refs/heads/main"] != second || e.refs(mirror)["refs/heads/main"] != second {
		t.Fatal("main was changed by a refused push")
	}
	// So is deleting it, and moving or deleting a protected tag.
	if out, err := gitCmd(work, "push", hub, ":main"); err == nil || !strings.Contains(out, "cannot be deleted") {
		t.Fatalf("deleting a protected branch: %v\n%s", err, out)
	}
	e.run(work, "tag", "-f", "v1.0", rewritten)
	if out, err := gitCmd(work, "push", "-f", hub, "v1.0"); err == nil || !strings.Contains(out, "the tag cannot be moved") {
		t.Fatalf("moving a protected tag: %v\n%s", err, out)
	}
	if out, err := gitCmd(work, "push", hub, ":refs/tags/v1.0"); err == nil || !strings.Contains(out, "cannot be deleted") {
		t.Fatalf("deleting a protected tag: %v\n%s", err, out)
	}
	// One refused ref refuses the whole push (nothing half-applied).
	if _, err := gitCmd(work, "push", "-f", hub, "main", "HEAD:refs/heads/other"); err == nil {
		t.Fatal("a push containing a refused update succeeded")
	}
	if _, ok := e.refs(e.hubRepo("acme/demo"))["refs/heads/other"]; ok {
		t.Fatal("part of a refused push was applied")
	}
	// Unprotected branches can still be rewritten and deleted.
	e.run(work, "push", "-q", "-f", hub, "HEAD:feature")
	e.run(work, "push", "-q", hub, ":feature")

	// Over SSH the same rules apply.
	if sshErr == nil {
		keyFile := filepath.Join(t.TempDir(), "dev")
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "dev", "-f", keyFile).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen: %v %s", err, out)
		}
		pub, _ := os.ReadFile(keyFile + ".pub")
		e.do(http.MethodPost, "/api/ssh-keys", map[string]any{"name": "dev", "public_key": string(pub), "scopes": []string{"write"}}, nil)
		cmd := exec.Command("git", "push", "-f", "ssh://git@"+e.srv.SSHAddr()+"/acme/demo.git", "main")
		cmd.Dir = work
		cmd.Env = append(append(os.Environ(), clientEnv()...), "GIT_SSH_COMMAND=ssh -i "+keyFile+" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o BatchMode=yes")
		b, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(b), "protected on the hub") {
			t.Fatalf("force-push over ssh: %v\n%s", err, b)
		}
	}

	// Lifting the rule allows the rewrite (and backs up what it replaces).
	e.do(http.MethodDelete, "/api/protections/"+strconv.FormatInt(rule.ID, 10), nil, nil)
	out = e.run(work, "push", "-f", hub, "main")
	mustContain(t, out, "under refs/backup/", "[ok]       mirror-a:demo")
	if e.refs(mirror)["refs/heads/main"] != rewritten {
		t.Fatal("the rewrite did not reach the mirror after the rule was lifted")
	}

	var page auditPage
	e.do(http.MethodGet, "/api/audit?action=protections", nil, &page)
	if page.find(func(r auditRow) bool { return strings.HasPrefix(r.Detail, "protect main in acme/*") }) == nil ||
		page.find(func(r auditRow) bool { return strings.HasPrefix(r.Detail, "unprotect main in acme/*") }) == nil {
		t.Fatalf("audit of protection changes: %+v", page.Entries)
	}
}
