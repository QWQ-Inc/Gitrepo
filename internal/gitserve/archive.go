package gitserve

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/qwq-inc/gitrepo/internal/registry"
)

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// ServeArchive serves GET /archive/{org}/{name}/{rev}.tar.gz (or .zip): a
// source snapshot for builds that need the code but not the history.
//
// Only commits reachable from a branch or tag are served, so content kept
// under refs/backup/ stays private. A full commit SHA names immutable
// content: for public repositories the response is cacheable forever (CDN),
// private ones get a private cache policy. Git LFS files appear as pointers.
func (s *Server) ServeArchive(w http.ResponseWriter, r *http.Request, org, name, file string) {
	format := ""
	rev := ""
	switch {
	case strings.HasSuffix(file, ".tar.gz"):
		format, rev = "tar.gz", strings.TrimSuffix(file, ".tar.gz")
	case strings.HasSuffix(file, ".tgz"):
		format, rev = "tar.gz", strings.TrimSuffix(file, ".tgz")
	case strings.HasSuffix(file, ".zip"):
		format, rev = "zip", strings.TrimSuffix(file, ".zip")
	default:
		http.Error(w, "gitrepo: use <rev>.tar.gz or <rev>.zip", http.StatusNotFound)
		return
	}
	org, name, err := registry.ParseSlug(org + "/" + name)
	if err != nil || rev == "" || strings.HasPrefix(rev, "-") || strings.ContainsAny(rev, " \t\n:^~?*[\\") || strings.Contains(rev, "..") {
		http.NotFound(w, r)
		return
	}
	slug := org + "/" + name

	p, err := s.Auth.FromRequest(r)
	if err != nil {
		unauthorized(w, "invalid token")
		return
	}
	repo, err := s.Access(r.Context(), slug, p, false, false)
	switch {
	case errors.Is(err, ErrUnauthenticated):
		unauthorized(w, "authentication required")
		return
	case errors.Is(err, ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, ErrForbidden):
		http.Error(w, "gitrepo: "+err.Error(), http.StatusForbidden)
		return
	case err != nil:
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	dir := s.Registry.Path(slug)
	git := func(args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, s.GitBin, append([]string{"--git-dir=" + dir}, args...)...)
		cmd.Env = append(os.Environ(), "HOME="+s.HomeDir, "XDG_CONFIG_HOME="+s.HomeDir, "GIT_CONFIG_NOSYSTEM=1")
		return cmd
	}
	out, err := git("rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}").Output()
	if err != nil {
		http.Error(w, "gitrepo: no such commit", http.StatusNotFound)
		return
	}
	sha := strings.TrimSpace(string(out))
	contains, err := git("for-each-ref", "--count=1", "--format=%(refname)", "--contains", sha, "refs/heads/", "refs/tags/").Output()
	if err != nil || len(strings.TrimSpace(string(contains))) == 0 {
		http.Error(w, "gitrepo: no such commit", http.StatusNotFound)
		return
	}

	etag := `"` + sha + "." + format + `"`
	h := w.Header()
	h.Set("ETag", etag)
	switch {
	case fullSHA.MatchString(rev) && repo.Visibility == "public":
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	case fullSHA.MatchString(rev):
		h.Set("Cache-Control", "private, max-age=31536000, immutable")
	default:
		h.Set("Cache-Control", "private, no-cache") // a branch or tag can move
	}
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	prefix := name + "-" + sha[:12]
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.%s"`, prefix, format))
	if format == "zip" {
		h.Set("Content-Type", "application/zip")
	} else {
		h.Set("Content-Type", "application/gzip")
	}
	if r.Method == http.MethodHead {
		return
	}

	gitFormat := "tar"
	if format == "zip" {
		gitFormat = "zip"
	}
	cmd := git("archive", "--format="+gitFormat, "--prefix="+prefix+"/", sha)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := cmd.Start(); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var dst io.Writer = w
	var gz *gzip.Writer
	if format == "tar.gz" {
		gz = gzip.NewWriter(w)
		dst = gz
	}
	_, copyErr := io.Copy(dst, stdout)
	if gz != nil && copyErr == nil {
		copyErr = gz.Close()
	}
	if copyErr != nil {
		cancel()
	}
	if err := cmd.Wait(); err != nil && copyErr == nil {
		s.Log.Warn("git archive", "repo", slug, "rev", sha, "err", err)
	}
}
