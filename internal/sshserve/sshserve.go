// Package sshserve serves hub repositories over SSH. Users authenticate
// with registered public keys that carry the same scopes and repository
// patterns as tokens; git-upload-pack / git-receive-pack run with exactly
// the authorization, hooks, push-to-create and policy of the HTTP endpoint
// (shared through gitserve.Server). git-lfs-authenticate hands out a
// short-lived token for the HTTP LFS API.
package sshserve

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/qwq-inc/gitrepo/internal/auth"
	"github.com/qwq-inc/gitrepo/internal/gitserve"
	"github.com/qwq-inc/gitrepo/internal/lfs"
	"github.com/qwq-inc/gitrepo/internal/registry"
	"github.com/qwq-inc/gitrepo/internal/store"
)

type Server struct {
	Git      *gitserve.Server
	Store    *store.Store
	Registry *registry.Registry
	Signer   *lfs.Signer
	// PublicURL is the HTTP base URL handed out by git-lfs-authenticate.
	PublicURL   string
	GitBin      string
	HostKeyPath string
	Log         *slog.Logger

	config  *ssh.ServerConfig
	hostKey ssh.Signer

	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// Init loads (or creates) the host key and prepares the SSH configuration.
func (s *Server) Init() error {
	key, err := loadOrCreateHostKey(s.HostKeyPath)
	if err != nil {
		return fmt.Errorf("ssh host key: %w", err)
	}
	s.hostKey = key
	s.config = &ssh.ServerConfig{
		PublicKeyCallback: s.checkKey,
		MaxAuthTries:      6,
		ServerVersion:     "SSH-2.0-Gitrepo",
	}
	s.config.AddHostKey(key)
	return nil
}

// HostKey returns the server's public host key in authorized_keys format.
func (s *Server) HostKey() string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.hostKey.PublicKey())))
}

// Fingerprint returns the SHA256 fingerprint of the host key.
func (s *Server) Fingerprint() string { return ssh.FingerprintSHA256(s.hostKey.PublicKey()) }

func loadOrCreateHostKey(path string) (ssh.Signer, error) {
	if raw, err := os.ReadFile(path); err == nil {
		return ssh.ParsePrivateKey(raw)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "gitrepo host key")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(priv)
}

const keyIDExt = "gitrepo-key-id"

func (s *Server) checkKey(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	k, err := s.Store.GetSSHKeyByFingerprint(context.Background(), ssh.FingerprintSHA256(key))
	if err != nil {
		return nil, errors.New("unknown public key")
	}
	stored, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k.PublicKey))
	if err != nil || string(stored.Marshal()) != string(key.Marshal()) {
		return nil, errors.New("unknown public key")
	}
	return &ssh.Permissions{Extensions: map[string]string{keyIDExt: strconv.FormatInt(k.ID, 10)}}, nil
}

// Serve accepts connections until Close.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.ln = ln
	s.conns = map[net.Conn]struct{}{}
	s.mu.Unlock()
	for {
		nc, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return err
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			nc.Close()
			return nil
		}
		s.conns[nc] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, nc)
				s.mu.Unlock()
				nc.Close()
			}()
			s.handleConn(nc)
		}()
	}
}

// Close stops the listener, drops connections and waits for sessions.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	if s.ln != nil {
		s.ln.Close()
	}
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	return nil
}

func (s *Server) handleConn(nc net.Conn) {
	nc.SetDeadline(time.Now().Add(30 * time.Second))
	conn, chans, reqs, err := ssh.NewServerConn(nc, s.config)
	if err != nil {
		return
	}
	defer conn.Close()
	nc.SetDeadline(time.Time{})
	go ssh.DiscardRequests(reqs)

	id, _ := strconv.ParseInt(conn.Permissions.Extensions[keyIDExt], 10, 64)
	p, err := s.principal(id)
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { conn.Wait(); cancel() }()

	var wg sync.WaitGroup
	for nch := range chans {
		if nch.ChannelType() != "session" {
			nch.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		ch, chReqs, err := nch.Accept()
		if err != nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.session(ctx, p, ch, chReqs)
		}()
	}
	wg.Wait()
}

func (s *Server) principal(keyID int64) (*auth.Principal, error) {
	keys, err := s.Store.ListSSHKeys(context.Background())
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		if k.ID == keyID {
			_ = s.Store.TouchSSHKey(context.Background(), k.ID)
			return &auth.Principal{Name: "ssh:" + k.Name, Scopes: k.Scopes, Repos: k.Repos}, nil
		}
	}
	return nil, store.ErrNotFound
}

func sendExit(ch ssh.Channel, code int) {
	ch.CloseWrite()
	ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
}

// session serves one channel: env (GIT_PROTOCOL only), exec, or a shell
// request answered with a friendly message. Requests keep being answered
// while a command runs.
func (s *Server) session(ctx context.Context, p *auth.Principal, ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	var gitProtocol string
	done := make(chan struct{})
	started := false
	for {
		select {
		case <-done:
			return
		case req, ok := <-reqs:
			if !ok {
				if started {
					<-done
				}
				return
			}
			switch {
			case req.Type == "env" && !started:
				var kv struct{ Name, Value string }
				ok := ssh.Unmarshal(req.Payload, &kv) == nil && kv.Name == "GIT_PROTOCOL"
				if ok {
					gitProtocol = kv.Value
				}
				req.Reply(ok, nil)
			case req.Type == "exec" && !started:
				var c struct{ Command string }
				if err := ssh.Unmarshal(req.Payload, &c); err != nil {
					req.Reply(false, nil)
					continue
				}
				req.Reply(true, nil)
				started = true
				go func() {
					defer close(done)
					code := s.exec(ctx, p, ch, c.Command, gitProtocol)
					sendExit(ch, code)
				}()
			case req.Type == "shell" && !started:
				req.Reply(true, nil)
				fmt.Fprintf(ch.Stderr(), "Hi %s! You've successfully authenticated, but Gitrepo does not provide shell access.\r\n", strings.TrimPrefix(p.Name, "ssh:"))
				sendExit(ch, 1)
				return
			default:
				if req.WantReply {
					req.Reply(false, nil)
				}
			}
		}
	}
}

// splitCommand splits a git-over-ssh command line, honouring single and
// double quotes (git quotes the path).
func splitCommand(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	in := false
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

func (s *Server) exec(ctx context.Context, p *auth.Principal, ch ssh.Channel, cmdline, gitProtocol string) int {
	stderr := ch.Stderr()
	fail := func(format string, args ...any) int {
		fmt.Fprintf(stderr, "gitrepo: "+format+"\n", args...)
		return 1
	}
	args := splitCommand(cmdline)
	if len(args) >= 2 && args[0] == "git" { // "git upload-pack 'x'" form
		args = append([]string{"git-" + args[1]}, args[2:]...)
	}
	if len(args) < 2 {
		return fail("unsupported command %q", cmdline)
	}
	verb := args[0]
	org, name, err := registry.ParseSlug(strings.TrimPrefix(args[1], "~/"))
	if err != nil {
		return fail("%v", err)
	}
	slug := org + "/" + name

	var write, create bool
	switch verb {
	case "git-upload-pack", "git-upload-archive":
	case "git-receive-pack":
		write, create = true, true
	case "git-lfs-authenticate":
		if len(args) < 3 || (args[2] != "upload" && args[2] != "download") {
			return fail("usage: git-lfs-authenticate <repo> <upload|download>")
		}
		write = args[2] == "upload"
	default:
		return fail("unsupported command %q (only git and git-lfs-authenticate are available)", verb)
	}
	repo, err := s.Git.Access(ctx, slug, p, write, create)
	if err != nil {
		if errors.Is(err, gitserve.ErrNotFound) {
			return fail("repository %s not found", slug)
		}
		return fail("%v", err)
	}

	if verb == "git-lfs-authenticate" {
		op := args[2]
		resp := map[string]any{
			"href":       strings.TrimRight(s.PublicURL, "/") + "/" + repo.Slug + ".git/info/lfs",
			"header":     map[string]string{"Authorization": "Bearer " + s.Signer.Sign(lfs.RepoKey(repo.Slug, repo.ID), lfs.AnyObject, op, time.Hour)},
			"expires_in": 3600,
		}
		json.NewEncoder(ch).Encode(resp)
		return 0
	}

	env := append([]string{"PATH=" + os.Getenv("PATH")}, s.Git.GitEnv(ctx, repo, p, write)...)
	if gitProtocol != "" {
		env = append(env, "GIT_PROTOCOL="+gitProtocol)
	}
	cmd := exec.CommandContext(ctx, s.GitBin, strings.TrimPrefix(verb, "git-"), s.Registry.Path(repo.Slug))
	cmd.Env = env
	cmd.WaitDelay = 10 * time.Second
	cmd.Stdout = ch
	cmd.Stderr = stderr
	// Copy stdin by hand: exec would otherwise wait for the client to close
	// its side, which it only does after receiving the exit status.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fail("internal error")
	}
	if err := cmd.Start(); err != nil {
		s.Log.Error("start git over ssh", "err", err)
		return fail("internal error")
	}
	go func() {
		io.Copy(stdin, ch)
		stdin.Close()
	}()
	err = cmd.Wait()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.ExitCode()
	default:
		s.Log.Warn("git over ssh", "cmd", verb, "repo", slug, "err", err)
		return 1
	}
}
