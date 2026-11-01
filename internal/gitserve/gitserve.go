// Package gitserve exposes hub repositories over Git's smart HTTP protocol by
// running the official `git http-backend`. It authenticates clients,
// creates repositories on first push, and passes policy to the hooks.
package gitserve

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/textproto"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/qwq-inc/gitrepo/internal/auth"
	"github.com/qwq-inc/gitrepo/internal/bundles"
	"github.com/qwq-inc/gitrepo/internal/gitexec"
	"github.com/qwq-inc/gitrepo/internal/githooks"
	"github.com/qwq-inc/gitrepo/internal/lfs"
	"github.com/qwq-inc/gitrepo/internal/provider"
	"github.com/qwq-inc/gitrepo/internal/registry"
	"github.com/qwq-inc/gitrepo/internal/store"
)

type Server struct {
	Store    *store.Store
	Registry *registry.Registry
	Auth     *auth.Authenticator
	Log      *slog.Logger

	GitBin     string
	ReposDir   string
	HooksDir   string
	HomeDir    string
	SocketPath string
	HookToken  string
	// MaxFileSize is the configured hard cap (0 = derive from remotes).
	MaxFileSize     int64
	PostReceiveWait time.Duration
	// OnCreate is called after a repository is created by a push.
	OnCreate func(repo *store.Repo)
	// LFS serves <repo>.git/info/lfs/*; its Auth is filled in per request.
	LFS *lfs.Server
	// Bundles, when set, are advertised to readers via bundle-uri and served
	// under <repo>.git/bundles/. Links to private bundles carry a token
	// signed with Signer.
	Bundles   *bundles.Manager
	Signer    *lfs.Signer
	PublicURL string
	// Git provides published-only views of repositories for archives.
	Git *gitexec.Git
	// TrustedProxies may set X-Forwarded-For (see ClientAddr).
	TrustedProxies []netip.Prefix
	// Gate refuses pushes while a repository is under maintenance.
	Gate *Gate
	// Upstream, on an edge node, is the hub: pushes are redirected there.
	Upstream string
	// Edge is the edge node's replica (nil on the hub).
	Edge Replica
}

// Replica is what the Git server needs from an edge node's replica.
type Replica interface {
	Ready() bool
	WaitFresh(ctx context.Context, slug string, max time.Duration)
}

const (
	svcUpload  = "git-upload-pack"
	svcReceive = "git-receive-pack"
)

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 3)
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	org, name, rest := parts[0], strings.TrimSuffix(parts[1], ".git"), parts[2]
	if _, _, err := registry.ParseSlug(org + "/" + name); err != nil {
		http.NotFound(w, r)
		return
	}
	slug := org + "/" + name

	if lfsPath, ok := strings.CutPrefix(rest, "info/lfs/"); ok {
		s.serveLFS(w, r, slug, lfsPath)
		return
	}
	if file, ok := strings.CutPrefix(rest, "bundles/"); ok && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		s.serveBundle(w, r, slug, file)
		return
	}

	var service string
	switch {
	case rest == "info/refs" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		service = r.URL.Query().Get("service")
		if service != svcUpload && service != svcReceive {
			http.Error(w, "gitrepo: the dumb HTTP protocol is not supported; use a git client with smart HTTP", http.StatusForbidden)
			return
		}
	case rest == svcUpload && r.Method == http.MethodPost:
		service = svcUpload
	case rest == svcReceive && r.Method == http.MethodPost:
		service = svcReceive
	default:
		http.NotFound(w, r)
		return
	}
	write := service == svcReceive

	if s.Upstream != "" {
		if write {
			// A read-only edge: git follows the redirect of its first
			// request and pushes to the hub.
			http.Redirect(w, r, strings.TrimRight(s.Upstream, "/")+r.URL.RequestURI(), http.StatusTemporaryRedirect)
			return
		}
		if s.Edge != nil && !s.Edge.Ready() {
			w.Header().Set("Retry-After", "5")
			http.Error(w, "gitrepo: this edge node is still loading its copy of the hub; retry shortly", http.StatusServiceUnavailable)
			return
		}
		if s.Edge != nil && rest == "info/refs" {
			s.Edge.WaitFresh(r.Context(), slug, 15*time.Second)
		}
	}

	p, err := s.Auth.FromRequest(r)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidToken) {
			unauthorized(w, "invalid token")
			return
		}
		s.Log.Error("authenticate", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()
	repo, err := s.Access(ctx, slug, p, write, write && rest == "info/refs")
	switch {
	case errors.Is(err, ErrUnauthenticated):
		unauthorized(w, "authentication required")
		return
	case errors.Is(err, ErrNotFound):
		http.Error(w, "gitrepo: repository not found", http.StatusNotFound)
		return
	case errors.Is(err, ErrForbidden):
		http.Error(w, "gitrepo: "+err.Error(), http.StatusForbidden)
		return
	case err != nil:
		s.Log.Error("repository access", "repo", slug, "err", err)
		http.Error(w, "gitrepo: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if write && s.Gate != nil {
		// Refusing at info/refs lets git show the reason to the user; the
		// POST is where the push actually happens, so it registers.
		if rest == "info/refs" {
			if s.Gate.Maintaining(repo.ID) {
				http.Error(w, "gitrepo: "+ErrMaintenance.Error(), http.StatusServiceUnavailable)
				return
			}
		} else {
			done, err := s.Gate.BeginPush(repo.ID)
			if err != nil {
				http.Error(w, "gitrepo: "+err.Error(), http.StatusServiceUnavailable)
				return
			}
			defer done()
		}
	}
	env := append([]string{
		"GIT_PROJECT_ROOT=" + s.ReposDir,
		"GIT_HTTP_EXPORT_ALL=1",
	}, s.GitEnv(ctx, repo, p, write, "http.receivepack", "true", "http.uploadpack", "true")...)
	env = append(env, githooks.EnvTransport+"=http", githooks.EnvClientAddr+"="+ClientAddr(r, s.TrustedProxies))
	// Hand http-backend exactly the service that was authorized: it reads
	// the last "service" parameter while authorization used the first.
	if rest == "info/refs" {
		r.URL.RawQuery = "service=" + service
	} else {
		r.URL.RawQuery = ""
	}
	RunBackend(w, r, s.GitBin, "/"+slug+".git/"+rest, env, s.Log)
}

// serveLFS hands Git LFS requests to the LFS server with an authorizer that
// applies the same token scopes and visibility rules as git itself. Like the
// git endpoints, it answers 401 rather than 404 to anonymous callers so the
// existence of private repositories is not revealed.
func (s *Server) serveLFS(w http.ResponseWriter, r *http.Request, slug, rest string) {
	lfsErr := func(status int, msg string) {
		w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
		if status == http.StatusUnauthorized {
			w.Header().Set("LFS-Authenticate", `Basic realm="gitrepo"`)
			w.Header().Set("WWW-Authenticate", `Basic realm="gitrepo"`)
		}
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"message":%q}`, "gitrepo: "+msg)
	}
	p, perr := s.Auth.FromRequest(r)
	if perr != nil && !strings.HasPrefix(auth.TokenFromRequest(r), "lfs1.") {
		lfsErr(http.StatusUnauthorized, "invalid credentials")
		return
	}
	repo, err := s.Store.GetRepoBySlug(r.Context(), slug)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if p == nil {
			lfsErr(http.StatusUnauthorized, "credentials required")
		} else {
			lfsErr(http.StatusNotFound, "repository not found")
		}
		return
	case err != nil:
		s.Log.Error("load repository", "repo", slug, "err", err)
		lfsErr(http.StatusInternalServerError, "internal error")
		return
	}
	authz := func(r *http.Request, slug string, write bool) error {
		if p == nil {
			if !write && repo.Visibility == "public" {
				return nil
			}
			return lfs.ErrUnauthorized
		}
		scope := auth.ScopeRead
		if write {
			scope = auth.ScopeWrite
		}
		if !p.Can(scope, slug) {
			return fmt.Errorf("%w: %s has no %s access to %s", lfs.ErrForbidden, p.Name, scope, slug)
		}
		return nil
	}
	srv := *s.LFS
	srv.Auth = authz
	srv.MaxObjectSize = s.maxLFSObjectSize(r.Context(), repo)
	srv.Serve(w, r, lfs.Repo{Slug: slug, ID: repo.ID, Dir: s.Registry.Path(slug)}, rest)
}

// Access errors.
var (
	ErrUnauthenticated = errors.New("authentication required")
	ErrForbidden       = errors.New("access denied")
	ErrNotFound        = errors.New("repository not found")
)

// Access resolves slug for a Git operation by p (nil = anonymous). write
// needs the write scope; reads of public repositories are open. With
// allowCreate, a missing repository is created when p has the create
// scope (push-to-create). Errors wrap ErrUnauthenticated, ErrForbidden or
// ErrNotFound.
func (s *Server) Access(ctx context.Context, slug string, p *auth.Principal, write, allowCreate bool) (*store.Repo, error) {
	repo, err := s.Store.GetRepoBySlug(ctx, slug)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if p == nil {
			return nil, ErrUnauthenticated
		}
		if !(write && allowCreate && p.Can(auth.ScopeCreate, slug)) {
			return nil, ErrNotFound
		}
		repo, _, err = s.Registry.CreateRepo(ctx, slug, "private", true)
		if err != nil {
			return nil, fmt.Errorf("cannot create repository: %w", err)
		}
		s.Log.Info("repository created by push", "repo", slug, "by", p.Name)
		if s.OnCreate != nil {
			s.OnCreate(repo)
		}
	case err != nil:
		return nil, err
	}
	if write {
		if p == nil {
			return nil, ErrUnauthenticated
		}
		if !p.Can(auth.ScopeWrite, slug) {
			return nil, fmt.Errorf("%w: %s has no write access to %s", ErrForbidden, p.Name, slug)
		}
	} else if repo.Visibility != "public" {
		if p == nil {
			return nil, ErrUnauthenticated
		}
		if !p.Can(auth.ScopeRead, slug) {
			return nil, fmt.Errorf("%w: %s has no read access to %s", ErrForbidden, p.Name, slug)
		}
	}
	return repo, nil
}

// GitEnv is the environment for upload-pack / receive-pack on repo: shared
// server-side git configuration (plus extraConfig key/value pairs), hook
// wiring and push policy. Configuration must be passed in one call: git
// reads a single GIT_CONFIG_COUNT.
func (s *Server) GitEnv(ctx context.Context, repo *store.Repo, p *auth.Principal, write bool, extraConfig ...string) []string {
	env := []string{
		"HOME=" + s.HomeDir,
		"XDG_CONFIG_HOME=" + s.HomeDir,
		githooks.EnvSocket + "=" + s.SocketPath,
		githooks.EnvHookToken + "=" + s.HookToken,
		githooks.EnvRepo + "=" + repo.Slug,
		githooks.EnvGit + "=" + s.GitBin,
		githooks.EnvWait + "=" + strconv.FormatInt(s.PostReceiveWait.Milliseconds(), 10),
	}
	if !write {
		extraConfig = append(extraConfig, s.bundleConfig(repo)...)
	}
	env = append(env, gitConfigEnv(append([]string{
		"core.hooksPath", s.HooksDir,
		"transfer.hideRefs", "refs/backup",
		"transfer.hideRefs", "refs/gitrepo",
		"receive.advertisePushOptions", "true",
		"receive.fsckObjects", "true",
		"uploadpack.allowFilter", "true",
	}, extraConfig...)...)...)
	if p != nil {
		env = append(env, "REMOTE_USER="+p.Name)
	}
	if write {
		env = append(env, githooks.EnvMaxFileSize+"="+strconv.FormatInt(s.maxFileSize(ctx, repo), 10))
	}
	return env
}

// bundleConfig advertises the repository's current bundle to readers.
func (s *Server) bundleConfig(repo *store.Repo) []string {
	if s.Bundles == nil {
		return nil
	}
	b, ok := s.Bundles.Current(repo.Slug, repo.ID)
	if !ok {
		return nil
	}
	uri := strings.TrimRight(s.PublicURL, "/") + "/" + repo.Slug + ".git/bundles/" + b.ID + ".bundle"
	if repo.Visibility != "public" {
		uri += "?t=" + s.Signer.Sign(lfs.RepoKey(repo.Slug, repo.ID), "bundle-"+b.ID, "bundle", 15*time.Minute)
	}
	return []string{
		"uploadpack.advertiseBundleURIs", "true",
		"bundle.version", "1",
		"bundle.mode", "all",
		"bundle.heuristic", "creationToken",
		"bundle.b" + b.ID + ".uri", uri,
		"bundle.b" + b.ID + ".creationToken", b.ID,
	}
}

// serveBundle serves <repo>.git/bundles/<id>.bundle to readers, or to
// anyone holding the token from the advertised link.
func (s *Server) serveBundle(w http.ResponseWriter, r *http.Request, slug, file string) {
	id, ok := strings.CutSuffix(file, ".bundle")
	if !ok || !bundles.ValidID(id) || s.Bundles == nil {
		http.NotFound(w, r)
		return
	}
	var repo *store.Repo
	if tok := r.URL.Query().Get("t"); tok != "" {
		var err error
		if repo, err = s.Store.GetRepoBySlug(r.Context(), slug); err != nil ||
			s.Signer.Verify(tok, lfs.RepoKey(slug, repo.ID), "bundle-"+id, "bundle") != nil {
			unauthorized(w, "invalid or expired bundle link")
			return
		}
	} else {
		p, err := s.Auth.FromRequest(r)
		if err != nil {
			unauthorized(w, "invalid token")
			return
		}
		repo, err = s.Access(r.Context(), slug, p, false, false)
		switch {
		case errors.Is(err, ErrUnauthenticated):
			unauthorized(w, "authentication required")
			return
		case err != nil:
			http.NotFound(w, r)
			return
		}
	}
	if !s.Bundles.Belongs(slug, id, repo.ID) {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(s.Bundles.Path(slug, id))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if repo.Visibility == "public" {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "private, max-age=86400")
	}
	w.Header().Set("Content-Type", "application/x-git-bundle")
	http.ServeContent(w, r, id+".bundle", fi.ModTime(), f)
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Basic realm="gitrepo"`)
	http.Error(w, "gitrepo: "+msg, http.StatusUnauthorized)
}

// gitConfigEnv injects configuration through GIT_CONFIG_COUNT (git ≥ 2.31),
// so every repository shares the same server-side settings.
func gitConfigEnv(kv ...string) []string {
	var env []string
	n := len(kv) / 2
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(n))
	for i := 0; i < n; i++ {
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[2*i]),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[2*i+1]))
	}
	return env
}

// maxFileSize is the strictest per-file limit among the configured cap and
// every remote the repository mirrors to.
func (s *Server) maxFileSize(ctx context.Context, repo *store.Repo) int64 {
	limit := s.MaxFileSize
	remotes, err := s.Store.ListRemotes(ctx, repo.ID)
	if err != nil {
		return limit
	}
	for _, rem := range remotes {
		t, err := s.Store.GetTarget(ctx, rem.TargetID)
		if err != nil {
			continue
		}
		l := t.MaxFileSize
		if l == 0 {
			if p, err := provider.Get(t.Provider); err == nil {
				l = p.MaxFileSize()
			}
		}
		if l > 0 && (limit == 0 || l < limit) {
			limit = l
		}
	}
	return limit
}

// maxLFSObjectSize is the strictest Git LFS object limit among the global
// setting and every mirror of the repository (target override, else the
// provider's known limit).
func (s *Server) maxLFSObjectSize(ctx context.Context, repo *store.Repo) int64 {
	limit := s.LFS.MaxObjectSize
	remotes, err := s.Store.ListRemotes(ctx, repo.ID)
	if err != nil {
		return limit
	}
	for _, rem := range remotes {
		t, err := s.Store.GetTarget(ctx, rem.TargetID)
		if err != nil {
			continue
		}
		l := t.MaxLFSObjectSize
		if l == 0 {
			if p, err := provider.Get(t.Provider); err == nil {
				if ll, ok := p.(provider.LFSLimiter); ok {
					l = ll.MaxLFSObjectSize()
				}
			}
		}
		if l > 0 && (limit == 0 || l < limit) {
			limit = l
		}
	}
	return limit
}

// RunBackend executes `git http-backend` as a CGI program, streaming the
// request body in and the response out. It is used instead of
// net/http/cgi because that package rejects chunked request bodies, which
// git sends for any push larger than http.postBuffer.
func RunBackend(w http.ResponseWriter, r *http.Request, gitBin, pathInfo string, extra []string, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	// http-backend emits headers before it has consumed the request body;
	// without full duplex, net/http would discard the unread body.
	_ = http.NewResponseController(w).EnableFullDuplex()

	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"GATEWAY_INTERFACE=CGI/1.1",
		"SERVER_PROTOCOL=" + r.Proto,
		"SERVER_SOFTWARE=gitrepo",
		"REQUEST_METHOD=" + r.Method,
		"QUERY_STRING=" + r.URL.RawQuery,
		"PATH_INFO=" + pathInfo,
		"REMOTE_ADDR=" + host,
		"CONTENT_TYPE=" + r.Header.Get("Content-Type"),
	}
	for _, k := range []string{"LANG", "LC_ALL", "TZ", "TMPDIR", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	if r.ContentLength > 0 {
		env = append(env, "CONTENT_LENGTH="+strconv.FormatInt(r.ContentLength, 10))
	}
	if ce := r.Header.Get("Content-Encoding"); ce != "" {
		env = append(env, "HTTP_CONTENT_ENCODING="+ce)
	}
	if gp := r.Header.Get("Git-Protocol"); gp != "" {
		env = append(env, "HTTP_GIT_PROTOCOL="+gp, "GIT_PROTOCOL="+gp)
	}
	env = append(env, extra...)

	cmd := exec.CommandContext(r.Context(), gitBin, "http-backend")
	cmd.Env = env
	cmd.WaitDelay = 10 * time.Second
	if r.Method == http.MethodPost {
		cmd.Stdin = r.Body
	}
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, n: 16 << 10}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := cmd.Start(); err != nil {
		log.Error("start http-backend", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	br := bufio.NewReaderSize(stdout, 32<<10)
	hdr, err := textproto.NewReader(br).ReadMIMEHeader()
	if err != nil {
		cmd.Wait()
		log.Error("http-backend produced no headers", "err", err, "stderr", stderr.String())
		http.Error(w, "gitrepo: git backend failed", http.StatusInternalServerError)
		return
	}
	status := http.StatusOK
	if st := hdr.Get("Status"); st != "" {
		if code, err := strconv.Atoi(strings.Fields(st)[0]); err == nil {
			status = code
		}
		hdr.Del("Status")
	}
	for k, vs := range hdr {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(status)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := br.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
	if err := cmd.Wait(); err != nil && r.Context().Err() == nil {
		log.Warn("http-backend exited with error", "path", pathInfo, "err", err, "stderr", strings.TrimSpace(stderr.String()))
	}
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	q := p
	if len(q) > l.n {
		q = q[:l.n]
	}
	l.n -= len(q)
	l.w.Write(q)
	return len(p), nil
}
