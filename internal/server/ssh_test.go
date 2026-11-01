package server_test

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwq-inc/gitrepo/internal/client"
	"github.com/qwq-inc/gitrepo/internal/config"
	"github.com/qwq-inc/gitrepo/internal/lfs"
)

func TestSSH(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("OpenSSH client not installed")
	}
	e := newEnv(t, func(c *config.Config) { c.SSH.Listen = "127.0.0.1:0" })
	addr := e.srv.SSHAddr()
	keys := t.TempDir()
	keygen := func(name string) (priv, pub string) {
		priv = filepath.Join(keys, name)
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", name, "-f", priv).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen: %v %s", err, out)
		}
		b, _ := os.ReadFile(priv + ".pub")
		return priv, string(b)
	}
	devKey, devPub := keygen("dev")
	ciKey, ciPub := keygen("ci")
	strangerKey, _ := keygen("stranger")
	e.do(http.MethodPost, "/api/ssh-keys", map[string]any{"name": "dev", "public_key": devPub, "scopes": []string{"create"}}, nil)
	e.do(http.MethodPost, "/api/ssh-keys", map[string]any{"name": "ci", "public_key": ciPub, "scopes": []string{"read"}}, nil)

	e.do(http.MethodPost, "/api/targets", map[string]any{
		"name": "mirror-a", "provider": "generic", "url_template": e.remoteURL + "/a/{name}.git",
		"credential": map[string]any{"kind": "basic", "username": userA, "secret": passA},
	}, nil)
	e.do(http.MethodPost, "/api/bindings", map[string]any{"org": "*", "target": "mirror-a"}, nil)
	if err := e.git.InitBare(t.Context(), e.remoteRepo("a/sshdemo.git"), "main"); err != nil {
		t.Fatal(err)
	}

	sshCommand := func(key string) string {
		return "ssh -i " + key + " -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o BatchMode=yes"
	}
	gitSSH := func(key, dir string, args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(append(os.Environ(), clientEnv()...), "GIT_SSH_COMMAND="+sshCommand(key))
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	url := "ssh://git@" + addr + "/acme/sshdemo.git"

	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	e.commit(work, "a.txt", "over ssh\n")

	t.Run("push over ssh creates the repository and syncs mirrors", func(t *testing.T) {
		out, err := gitSSH(devKey, work, "push", url, "main")
		if err != nil {
			t.Fatalf("push: %v\n%s", err, out)
		}
		mustContain(t, out, "[ok]       mirror-a:sshdemo")
		if e.refs(e.remoteRepo("a/sshdemo.git"))["refs/heads/main"] == "" {
			t.Fatal("mirror not updated")
		}
		var r client.Repo
		e.do(http.MethodGet, "/api/repos/acme/sshdemo", nil, &r)
		if r.SSHURL != url {
			t.Fatalf("ssh_url = %q, want %q", r.SSHURL, url)
		}
		var page auditPage
		e.do(http.MethodGet, "/api/audit?repo=acme/sshdemo", nil, &page)
		if len(page.Entries) != 1 || page.Entries[0].Actor != "ssh:dev" || page.Entries[0].Via != "ssh" ||
			page.Entries[0].ClientAddr != "127.0.0.1" || !strings.HasPrefix(page.Entries[0].Detail, "main created at ") {
			t.Fatalf("audit of the ssh push: %+v", page.Entries)
		}
	})

	t.Run("a read-only key can clone (protocol v2) but not push", func(t *testing.T) {
		clone := filepath.Join(t.TempDir(), "clone")
		if out, err := gitSSH(ciKey, "", "-c", "protocol.version=2", "clone", "-q", url, clone); err != nil {
			t.Fatalf("clone: %v\n%s", err, out)
		}
		out, err := gitSSH(ciKey, clone, "push", url, "HEAD:refs/heads/ci")
		if err == nil {
			t.Fatalf("read-only push succeeded:\n%s", out)
		}
		mustContain(t, out, "ssh:ci has no write access to acme/sshdemo")
	})

	t.Run("hidden backup content cannot be read through git archive --remote", func(t *testing.T) {
		hubDir := e.hubRepo("acme/sshdemo")
		secret, err := e.git.RunInput(t.Context(), hubDir, []byte("TOPSECRET\n"), "hash-object", "-w", "--stdin")
		if err != nil {
			t.Fatal(err)
		}
		tree, err := e.git.RunInput(t.Context(), hubDir, []byte("100644 blob "+strings.TrimSpace(string(secret))+"\tsecret.txt\n"), "mktree")
		if err != nil {
			t.Fatal(err)
		}
		commit, err := e.git.Run(t.Context(), hubDir, "commit-tree", strings.TrimSpace(string(tree)), "-m", "secret")
		if err != nil {
			t.Fatal(err)
		}
		ref := "refs/backup/20260101T000000.000Z/refs/heads/main"
		e.git.UpdateRefs(t.Context(), hubDir, []string{"update " + ref + " " + strings.TrimSpace(string(commit))})
		out, err := gitSSH(ciKey, "", "archive", "--remote="+url, ref, "secret.txt")
		if err == nil || strings.Contains(out, "TOPSECRET") {
			t.Fatalf("backup content readable via upload-archive: %v\n%s", err, out)
		}
	})

	t.Run("a deleted key stops working on an open connection", func(t *testing.T) {
		tmpKey, tmpPub := keygen("temp")
		var k struct {
			ID int64 `json:"id"`
		}
		e.do(http.MethodPost, "/api/ssh-keys", map[string]any{"name": "temp", "public_key": tmpPub, "scopes": []string{"read"}}, &k)
		host, port, _ := strings.Cut(addr, ":")
		ctl := filepath.Join(keys, "ctl")
		master := exec.Command("ssh", append(strings.Fields(sshCommand(tmpKey))[1:], "-M", "-S", ctl, "-o", "ControlPersist=60", "-fN", "-p", port, "git@"+host)...)
		if out, err := master.CombinedOutput(); err != nil {
			t.Fatalf("control master: %v %s", err, out)
		}
		defer exec.Command("ssh", "-S", ctl, "-O", "exit", "git@"+host).Run()
		viaMaster := func() (string, error) {
			cmd := exec.Command("git", "ls-remote", url)
			cmd.Env = append(append(os.Environ(), clientEnv()...), "GIT_SSH_COMMAND=ssh -S "+ctl+" -o ControlMaster=no -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR")
			out, err := cmd.CombinedOutput()
			return string(out), err
		}
		if out, err := viaMaster(); err != nil {
			t.Fatalf("multiplexed ls-remote: %v %s", err, out)
		}
		e.do(http.MethodDelete, "/api/ssh-keys/"+itoa(k.ID), nil, nil)
		if out, err := viaMaster(); err == nil {
			t.Fatalf("a deleted key still works on the open connection:\n%s", out)
		}
	})

	t.Run("unknown keys are rejected", func(t *testing.T) {
		out, err := gitSSH(strangerKey, "", "ls-remote", url)
		if err == nil || !strings.Contains(out, "Permission denied") {
			t.Fatalf("unknown key: %v\n%s", err, out)
		}
	})

	t.Run("an interactive login gets a friendly refusal", func(t *testing.T) {
		host, port, _ := strings.Cut(addr, ":")
		args := append(strings.Fields(sshCommand(devKey))[1:], "-T", "-p", port, "git@"+host)
		out, err := exec.Command("ssh", args...).CombinedOutput()
		if err == nil || !strings.Contains(string(out), "Hi dev! You've successfully authenticated, but Gitrepo does not provide shell access.") {
			t.Fatalf("shell: %v\n%s", err, out)
		}
	})

	t.Run("git-lfs works over ssh through git-lfs-authenticate", func(t *testing.T) {
		if _, err := gitCmd("", "lfs", "version"); err != nil {
			t.Skip("git-lfs is not installed")
		}
		repo := filepath.Join(t.TempDir(), "lfsrepo")
		e.run("", "init", "-q", "-b", "main", repo)
		e.run(repo, "lfs", "install", "--local")
		e.run(repo, "lfs", "track", "*.bin")
		os.WriteFile(filepath.Join(repo, "data.bin"), []byte(strings.Repeat("lfs over ssh ", 4096)), 0o644)
		e.run(repo, "add", ".gitattributes", "data.bin")
		e.run(repo, "commit", "-q", "-m", "lfs")
		oid := strings.Fields(e.run(repo, "lfs", "ls-files", "--long"))[0]
		lfsURL := "ssh://git@" + addr + "/acme/sshlfs.git"
		if out, err := gitSSH(devKey, repo, "push", lfsURL, "main"); err != nil {
			t.Fatalf("push: %v\n%s", err, out)
		}
		if !lfs.Has(e.hubRepo("acme/sshlfs"), oid, -1) {
			t.Fatal("LFS object did not reach the hub over ssh")
		}
	})
}
