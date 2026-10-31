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
	"net/textproto"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/qwq-inc/gitrepo/internal/auth"
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
	repo, err := s.Store.GetRepoBySlug(ctx, slug)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if p == nil {
			unauthorized(w, "authentication required")
			return
		}
		if !(write && rest == "info/refs" && p.Can(auth.ScopeCreate, slug)) {
			http.Error(w, "gitrepo: repository not found", http.StatusNotFound)
			return
		}
		repo, _, err = s.Registry.CreateRepo(ctx, slug, "private", true)
		if err != nil {
			s.Log.Error("create repository on push", "repo", slug, "err", err)
			http.Error(w, "gitrepo: cannot create repository: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.Log.Info("repository created by push", "repo", slug, "by", p.Name)
		if s.OnCreate != nil {
			s.OnCreate(repo)
		}
	case err != nil:
		s.Log.Error("load repository", "repo", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if write {
		if p == nil {
			unauthorized(w, "authentication required")
			return
		}
		if !p.Can(auth.ScopeWrite, slug) {
			http.Error(w, "gitrepo: token has no write access to "+slug, http.StatusForbidden)
			return
		}
	} else if repo.Visibility != "public" {
		if p == nil {
			unauthorized(w, "authentication required")
			return
		}
		if !p.Can(auth.ScopeRead, slug) {
			http.Error(w, "gitrepo: token has no read access to "+slug, http.StatusForbidden)
			return
		}
	}

	env := []string{
		"GIT_PROJECT_ROOT=" + s.ReposDir,
		"GIT_HTTP_EXPORT_ALL=1",
		"HOME=" + s.HomeDir,
		"XDG_CONFIG_HOME=" + s.HomeDir,
		githooks.EnvSocket + "=" + s.SocketPath,
		githooks.EnvHookToken + "=" + s.HookToken,
		githooks.EnvRepo + "=" + slug,
		githooks.EnvGit + "=" + s.GitBin,
		githooks.EnvWait + "=" + strconv.FormatInt(s.PostReceiveWait.Milliseconds(), 10),
	}
	env = append(env, gitConfigEnv(
		"core.hooksPath", s.HooksDir,
		"transfer.hideRefs", "refs/backup",
		"receive.advertisePushOptions", "true",
		"receive.fsckObjects", "true",
		"uploadpack.allowFilter", "true",
		"http.receivepack", "true",
		"http.uploadpack", "true",
	)...)
	if p != nil {
		env = append(env, "REMOTE_USER="+p.Name)
	}
	if write {
		env = append(env, githooks.EnvMaxFileSize+"="+strconv.FormatInt(s.maxFileSize(ctx, repo), 10))
	}
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
// applies the same token scopes and visibility rules as git itself.
func (s *Server) serveLFS(w http.ResponseWriter, r *http.Request, slug, rest string) {
	repo, err := s.Store.GetRepoBySlug(r.Context(), slug)
	if err != nil {
		w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"gitrepo: repository not found"}`)
		return
	}
	authz := func(r *http.Request, slug string, write bool) error {
		p, err := s.Auth.FromRequest(r)
		if err != nil {
			return lfs.ErrUnauthorized
		}
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
			return fmt.Errorf("%w: token has no %s access to %s", lfs.ErrForbidden, scope, slug)
		}
		return nil
	}
	srv := *s.LFS
	srv.Auth = authz
	srv.Serve(w, r, lfs.Repo{Slug: slug, Dir: s.Registry.Path(slug)}, rest)
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
