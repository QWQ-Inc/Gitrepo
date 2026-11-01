package server_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwq-inc/gitrepo/internal/client"
	"github.com/qwq-inc/gitrepo/internal/config"
)

// The pre-receive and post-receive hooks parse their stdin with
// strings.Fields, which also splits on Unicode spaces (U+00A0, U+0085,
// U+2000…). Git allows those bytes in ref names, so an update of such a ref
// is silently dropped by readUpdates: branch protection, the
// heads/tags-only rule, the file size limit and the post-receive backup
// never see it.
func TestUnicodeSpaceRefNamesAreChecked(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Policy.MaxFileSize = 1024 })
	var dev client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	e.do(http.MethodPost, "/api/protections", map[string]any{"repos": "*", "ref": "release/*"}, nil)
	hub := e.hubURL(dev.Token, "acme/demo")

	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	first := e.commit(work, "a.txt", "1\n")
	e.run(work, "push", "-q", hub, "main")

	// Control: an ordinary protected branch cannot be rewritten.
	e.run(work, "push", "-q", hub, "HEAD:refs/heads/release/1.0")
	second := e.commit(work, "a.txt", "2\n")
	e.run(work, "push", "-q", hub, "HEAD:refs/heads/release/1.0")
	e.run(work, "reset", "-q", "--hard", first)
	rewritten := e.commit(work, "b.txt", "rewritten\n")
	if out, err := gitCmd(work, "push", "-f", hub, "HEAD:refs/heads/release/1.0"); err == nil {
		t.Fatalf("control: force-push to release/1.0 succeeded:\n%s", out)
	}

	// The same branch pattern with a no-break space inside the name.
	nbsp := "refs/heads/release/1.0 lts"
	e.run(work, "push", "-q", hub, second+":"+nbsp)
	out, err := gitCmd(work, "push", "-f", hub, rewritten+":"+nbsp)
	t.Logf("force-push to %q: err=%v\n%s", nbsp, err, out)
	refs := e.allRefs(e.hubRepo("acme/demo"))
	if refs[nbsp] == rewritten {
		t.Errorf("BUG: protected ref %q (matches release/*) was force-pushed on the hub", nbsp)
	}
	backedUp := false
	for r := range refs {
		if strings.HasPrefix(r, "refs/backup/") && strings.Contains(r, "1.0 lts") {
			backedUp = true
		}
	}
	if refs[nbsp] == rewritten && !backedUp {
		t.Errorf("BUG: the overwritten value %s of %q was not backed up (post-receive never saw the update)", second[:12], nbsp)
	}
	if out, err := gitCmd(work, "push", hub, ":"+nbsp); err == nil {
		t.Errorf("BUG: protected ref %q was deleted:\n%s", nbsp, out)
	}

	// Refs outside refs/heads and refs/tags, and oversized files.
	if out, err := gitCmd(work, "push", hub, "HEAD:refs/notes/x y"); err == nil {
		t.Errorf("BUG: a push to refs/notes/x<NBSP>y was accepted:\n%s", out)
	}
	big := make([]byte, 64<<10)
	for i := range big {
		big[i] = byte('a' + i%26)
	}
	if err := os.WriteFile(filepath.Join(work, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	e.run(work, "add", "big.bin")
	e.run(work, "commit", "-q", "-m", "big")
	if out, err := gitCmd(work, "push", hub, "HEAD:refs/heads/big"); err == nil {
		t.Fatalf("control: a 64 KiB file passed the 1 KiB limit:\n%s", out)
	}
	if out, err := gitCmd(work, "push", hub, "HEAD:refs/heads/big file"); err == nil {
		t.Errorf("BUG: a 64 KiB file was accepted despite the 1 KiB limit via refs/heads/big<NBSP>file:\n%s", out)
	}
}
