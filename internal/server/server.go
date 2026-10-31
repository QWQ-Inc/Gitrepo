// Package server wires Gitrepo together: the public HTTP listener (Git smart
// HTTP + admin API + metrics), the internal unix socket used by hooks and
// GIT_ASKPASS, the sync manager and the background patrol/retry loops.
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qwq-inc/gitrepo/internal/auth"
	"github.com/qwq-inc/gitrepo/internal/config"
	"github.com/qwq-inc/gitrepo/internal/gitexec"
	"github.com/qwq-inc/gitrepo/internal/gitserve"
	"github.com/qwq-inc/gitrepo/internal/lfs"
	"github.com/qwq-inc/gitrepo/internal/notify"
	"github.com/qwq-inc/gitrepo/internal/registry"
	"github.com/qwq-inc/gitrepo/internal/secrets"
	"github.com/qwq-inc/gitrepo/internal/sshserve"
	"github.com/qwq-inc/gitrepo/internal/store"
	"github.com/qwq-inc/gitrepo/internal/syncer"
	"github.com/qwq-inc/gitrepo/internal/webui"
)

// Options tune behaviour that is not part of the config file.
type Options struct {
	// NoBackground disables the startup pass and the patrol/retry loops
	// (used by tests that want deterministic sync runs).
	NoBackground bool
}

type Server struct {
	cfg  *config.Config
	opts Options
	log  *slog.Logger

	store  *store.Store
	git    *gitexec.Git
	box    *secrets.Box
	reg    *registry.Registry
	mgr    *syncer.Manager
	notify *notify.Notifier
	ssh    *sshserve.Server // nil when SSH is disabled
	sshLn  net.Listener
	auth   *auth.Authenticator
	gitsrv *gitserve.Server

	tickets    *gitexec.Tickets
	hookToken  string
	socketPath string
	selfPath   string

	httpSrv *http.Server
	sockSrv *http.Server
	cancel  context.CancelFunc
	done    chan struct{}

	expiryMu       sync.Mutex
	expiryNotified map[int64]time.Time
}

// New prepares the data directory, database and components.
func New(cfg *config.Config, log *slog.Logger, opts Options) (*Server, error) {
	key, err := loadSecretKey(cfg)
	if err != nil {
		return nil, err
	}
	lfsSigner := lfs.NewSigner(key)
	box, err := secrets.New(key)
	if err != nil {
		return nil, err
	}
	for _, d := range []string{cfg.DataDir, cfg.ReposDir(), cfg.HooksDir(), cfg.BinDir(), cfg.RunDir(), cfg.TmpDir(), cfg.HomeDir(), cfg.SSHDir(), cfg.TrashDir()} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, err
		}
	}
	os.Chmod(cfg.TmpDir(), 0o700)
	os.Chmod(cfg.SSHDir(), 0o700)

	self := cfg.Git.SelfPath
	if self == "" {
		if self, err = os.Executable(); err != nil {
			return nil, fmt.Errorf("locate gitrepo executable: %w", err)
		}
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	s := &Server{
		cfg: cfg, opts: opts, log: log, store: st, box: box,
		tickets:    gitexec.NewTickets(),
		hookToken:  randomHex(24),
		socketPath: socketPath(cfg),
		selfPath:   self,
		done:       make(chan struct{}),

		expiryNotified: map[int64]time.Time{},
	}
	s.git = &gitexec.Git{
		Bin:            cfg.Git.Binary,
		Env:            []string{"HOME=" + cfg.HomeDir(), "XDG_CONFIG_HOME=" + cfg.HomeDir()},
		TmpDir:         cfg.TmpDir(),
		KnownHostsFile: filepath.Join(cfg.SSHDir(), "known_hosts"),
		AskpassPath:    filepath.Join(cfg.BinDir(), "askpass"),
		SocketPath:     s.socketPath,
		Tickets:        s.tickets,
	}
	if err := s.checkGit(); err != nil {
		st.Close()
		return nil, err
	}
	if err := s.writeShims(); err != nil {
		st.Close()
		return nil, err
	}
	s.reg = &registry.Registry{Store: st, Git: s.git, ReposDir: cfg.ReposDir(), TrashDir: cfg.TrashDir()}
	s.notify = notify.New(st, box, log)
	s.mgr = syncer.New(st, s.git, box, s.reg, log, syncer.Options{
		Workers:            cfg.Sync.Workers,
		PushTimeout:        cfg.Sync.PushTimeout.D(),
		RetryBase:          cfg.Sync.RetryBase.D(),
		RetryMax:           cfg.Sync.RetryMax.D(),
		AlertAfterFailures: cfg.Alerts.FailureThreshold,
		PublicURL:          cfg.PublicURL,
		Notify:             s.notify.Emit,
	})
	adminHash := ""
	if tok := strings.TrimSpace(os.Getenv(cfg.AdminTokenEnv)); tok != "" {
		adminHash = auth.Hash(tok)
	} else {
		log.Warn("no admin token configured; the API is only usable with tokens created earlier", "env", cfg.AdminTokenEnv)
	}
	s.auth = &auth.Authenticator{Store: st, AdminHash: adminHash}
	s.gitsrv = &gitserve.Server{
		Store: st, Registry: s.reg, Auth: s.auth, Log: log,
		GitBin:          cfg.Git.Binary,
		ReposDir:        cfg.ReposDir(),
		HooksDir:        cfg.HooksDir(),
		HomeDir:         cfg.HomeDir(),
		SocketPath:      s.socketPath,
		HookToken:       s.hookToken,
		MaxFileSize:     int64(cfg.Policy.MaxFileSize),
		PostReceiveWait: cfg.Sync.PostReceiveWait.D(),
		LFS:             &lfs.Server{Signer: lfsSigner, MaxObjectSize: int64(cfg.Policy.MaxLFSObjectSize), Log: log},
	}
	if cfg.SSH.Listen != "" {
		s.ssh = &sshserve.Server{
			Git: s.gitsrv, Store: st, Registry: s.reg, Signer: lfsSigner,
			PublicURL: cfg.PublicURL, GitBin: cfg.Git.Binary, Log: log,
			HostKeyPath: filepath.Join(cfg.SSHDir(), "host_ed25519_key"),
		}
		if err := s.ssh.Init(); err != nil {
			st.Close()
			return nil, err
		}
	}
	return s, nil
}

// SSHAddr is the address the SSH server listens on ("" when disabled).
func (s *Server) SSHAddr() string {
	if s.sshLn == nil {
		return ""
	}
	return s.sshLn.Addr().String()
}

// sshHost is the host[:port] used in ssh:// clone URLs.
func (s *Server) sshHost() string {
	if s.ssh == nil {
		return ""
	}
	if s.cfg.SSH.PublicHost != "" {
		return s.cfg.SSH.PublicHost
	}
	host := "localhost"
	if u, err := url.Parse(s.cfg.PublicURL); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	_, port, _ := net.SplitHostPort(s.cfg.SSH.Listen)
	if s.sshLn != nil {
		_, port, _ = net.SplitHostPort(s.sshLn.Addr().String())
	}
	if port == "" || port == "22" {
		return host
	}
	return net.JoinHostPort(host, port)
}

// SSHCloneURL returns the ssh:// URL of a repository ("" when disabled).
func (s *Server) SSHCloneURL(slug string) string {
	if h := s.sshHost(); h != "" {
		return "ssh://git@" + h + "/" + slug + ".git"
	}
	return ""
}

func loadSecretKey(cfg *config.Config) ([]byte, error) {
	raw := os.Getenv(cfg.SecretKeyEnv)
	if raw == "" && cfg.SecretKeyFile != "" {
		b, err := os.ReadFile(cfg.SecretKeyFile)
		if err != nil {
			return nil, fmt.Errorf("read secret key file: %w", err)
		}
		raw = string(b)
	}
	if raw == "" {
		return nil, fmt.Errorf("no secret key: set %s (generate one with `gitrepo keygen`)", cfg.SecretKeyEnv)
	}
	return secrets.ParseKey(raw)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// socketPath keeps the unix socket path under the OS limit (~104 bytes).
func socketPath(cfg *config.Config) string {
	p := filepath.Join(cfg.RunDir(), "gitrepo.sock")
	if len(p) < 100 {
		return p
	}
	sum := sha256.Sum256([]byte(cfg.DataDir))
	return filepath.Join(os.TempDir(), "gitrepo-"+hex.EncodeToString(sum[:6])+".sock")
}

func (s *Server) checkGit() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	major, minor, err := s.git.Version(ctx)
	if err != nil {
		return fmt.Errorf("git is required: %w", err)
	}
	if major < 2 || (major == 2 && minor < 31) {
		return fmt.Errorf("git >= 2.31 is required, found %d.%d", major, minor)
	}
	return nil
}

// writeShims installs the hook and askpass entry points. They are tiny
// shell scripts so git can exec them regardless of how Gitrepo is installed.
func (s *Server) writeShims() error {
	q := "'" + strings.ReplaceAll(s.selfPath, "'", `'\''`) + "'"
	shims := map[string]string{
		filepath.Join(s.cfg.HooksDir(), "pre-receive"):  "#!/bin/sh\nexec " + q + " hook pre-receive\n",
		filepath.Join(s.cfg.HooksDir(), "post-receive"): "#!/bin/sh\nexec " + q + " hook post-receive\n",
		filepath.Join(s.cfg.BinDir(), "askpass"):        "#!/bin/sh\nexec " + q + " askpass \"$@\"\n",
	}
	for path, body := range shims {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(body), 0o755); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
	}
	return nil
}

// Handler returns the public HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerAPI(mux)
	mux.Handle("GET /ui/", webui.Handler("/ui/"))
	mux.Handle("GET /ui", http.RedirectHandler("/ui/", http.StatusFound))
	mux.Handle("GET /{$}", http.RedirectHandler("/ui/", http.StatusFound))
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.requireAdmin(s.handleMetrics))
	mux.Handle("/", s.gitsrv)
	return logRequests(s.log, mux)
}

// Start begins serving on ln and starts background work.
func (s *Server) Start(ln net.Listener) error {
	os.Remove(s.socketPath)
	sock, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen on internal socket: %w", err)
	}
	if err := os.Chmod(s.socketPath, 0o600); err != nil {
		sock.Close()
		return err
	}
	s.sockSrv = &http.Server{Handler: s.internalHandler(), ReadHeaderTimeout: 10 * time.Second}
	s.httpSrv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		if err := s.sockSrv.Serve(sock); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("internal socket server", "err", err)
		}
	}()
	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("http server", "err", err)
		}
	}()
	if s.ssh != nil {
		sshLn, err := net.Listen("tcp", s.cfg.SSH.Listen)
		if err != nil {
			return fmt.Errorf("listen for ssh: %w", err)
		}
		s.sshLn = sshLn
		go func() {
			if err := s.ssh.Serve(sshLn); err != nil {
				s.log.Error("ssh server", "err", err)
			}
		}()
		s.log.Info("ssh enabled", "listen", sshLn.Addr().String(), "host_key", s.ssh.Fingerprint())
	}
	s.notify.Start(2)
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	if s.opts.NoBackground {
		close(s.done)
	} else {
		go s.background(ctx)
	}
	return nil
}

// Shutdown stops accepting requests, cancels syncs and closes the database.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.cancel != nil {
		s.cancel()
	}
	var errs []error
	if s.httpSrv != nil {
		errs = append(errs, s.httpSrv.Shutdown(ctx))
	}
	if s.ssh != nil {
		s.ssh.Close()
	}
	s.mgr.Shutdown()
	s.notify.Stop()
	if s.sockSrv != nil {
		errs = append(errs, s.sockSrv.Shutdown(ctx))
	}
	<-s.done
	os.Remove(s.socketPath)
	errs = append(errs, s.store.Close())
	return errors.Join(errs...)
}

// Manager exposes the sync manager (tests).
func (s *Server) Manager() *syncer.Manager { return s.mgr }

// Store exposes the store (tests).
func (s *Server) Store() *store.Store { return s.store }

func (s *Server) background(ctx context.Context) {
	defer close(s.done)
	startup := time.NewTimer(3 * time.Second)
	patrol := time.NewTicker(s.cfg.Sync.PatrolInterval.D())
	retry := time.NewTicker(15 * time.Second)
	defer startup.Stop()
	defer patrol.Stop()
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-startup.C:
			s.patrol(ctx)
		case <-patrol.C:
			s.patrol(ctx)
		case <-retry.C:
			s.retryDue(ctx)
		}
	}
}

func (s *Server) patrol(ctx context.Context) {
	if err := s.mgr.EnqueueAll(ctx, syncer.TriggerPatrol); err != nil {
		s.log.Error("patrol", "err", err)
		return
	}
	repos, err := s.store.ListRepos(ctx)
	if err != nil {
		return
	}
	for _, r := range repos {
		if n, err := syncer.PruneBackups(ctx, s.git, s.reg.Path(r.Slug), s.cfg.Sync.BackupRetention.D()); err != nil {
			s.log.Warn("prune backups", "repo", r.Slug, "err", err)
		} else if n > 0 {
			s.log.Info("pruned backup refs", "repo", r.Slug, "count", n)
		}
	}
	if err := s.store.PruneRuns(ctx, 200); err != nil {
		s.log.Warn("prune sync runs", "err", err)
	}
	s.checkCredentialExpiry(ctx)
}

// checkCredentialExpiry emits credential.expiring at most once a day per
// target whose credential expires within the warning window.
func (s *Server) checkCredentialExpiry(ctx context.Context) {
	targets, err := s.store.ListTargets(ctx)
	if err != nil {
		return
	}
	now := time.Now()
	window := s.cfg.Alerts.CredentialExpiryWarning.D()
	s.expiryMu.Lock()
	defer s.expiryMu.Unlock()
	for _, t := range targets {
		if t.ExpiresAt == 0 {
			continue
		}
		exp := store.ToTime(t.ExpiresAt)
		if exp.Sub(now) > window {
			continue
		}
		if last, ok := s.expiryNotified[t.ID]; ok && now.Sub(last) < 24*time.Hour {
			continue
		}
		s.expiryNotified[t.ID] = now
		expUTC := exp.UTC()
		s.notify.Emit(notify.Event{Event: notify.EventCredentialExpiring, Target: t.Name, ExpiresAt: &expUTC})
		s.log.Warn("target credential expires soon", "target", t.Name, "expires_at", expUTC)
	}
}

func (s *Server) retryDue(ctx context.Context) {
	due, err := s.store.DueRetries(ctx, store.Now())
	if err != nil {
		s.log.Error("load due retries", "err", err)
		return
	}
	byRepo := map[int64][]int64{}
	for _, r := range due {
		byRepo[r.RepoID] = append(byRepo[r.RepoID], r.ID)
	}
	for repoID, ids := range byRepo {
		s.mgr.Enqueue(repoID, syncer.Request{Trigger: syncer.TriggerRetry, RemoteIDs: ids})
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok\n"))
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", rec.status, "dur", time.Since(start).Round(time.Millisecond))
	})
}
