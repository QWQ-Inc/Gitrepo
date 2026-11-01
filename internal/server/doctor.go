package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/qwq-inc/gitrepo/internal/provider"
	"github.com/qwq-inc/gitrepo/internal/store"
)

// GET /api/doctor checks a deployment from the server's own point of view:
// tools, the public URL, disk space, every target's credential against
// its platform, failing mirrors and webhooks, SSH and edge nodes.

type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail"`
}

func (s *Server) registerDoctorAPI(h func(string, http.HandlerFunc)) {
	h("GET /api/doctor", s.apiDoctor)
}

func (s *Server) apiDoctor(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	var mu sync.Mutex
	var checks []doctorCheck
	add := func(name, status, format string, args ...any) {
		mu.Lock()
		checks = append(checks, doctorCheck{Name: name, Status: status, Detail: fmt.Sprintf(format, args...)})
		mu.Unlock()
	}

	if major, minor, err := s.git.Version(ctx); err != nil {
		add("git", "fail", "%v", err)
	} else {
		add("git", "ok", "git %d.%d", major, minor)
	}
	if s.git.LFSAvailable(ctx) {
		add("git-lfs", "ok", "installed")
	} else {
		add("git-lfs", "warn", "not installed: repositories using Git LFS cannot be synced to mirrors or imported")
	}
	if s.auth.AdminHash == "" {
		add("admin token", "warn", "no bootstrap admin token configured (%s); only tokens created earlier work", s.cfg.AdminTokenEnv)
	} else {
		add("admin token", "ok", "configured")
	}
	s.checkPublicURL(ctx, add)
	s.checkDisk(add)

	var wg sync.WaitGroup
	targets, err := s.store.ListTargets(ctx)
	if err != nil {
		add("targets", "fail", "%v", err)
	}
	if len(targets) == 0 {
		add("targets", "warn", "no platform credentials yet: add one with gr target add")
	}
	for _, t := range targets {
		wg.Add(1)
		go func(t *store.Target) {
			defer wg.Done()
			s.checkTarget(ctx, t, add)
		}(t)
	}
	wg.Wait()
	s.checkMirrors(ctx, add)
	s.checkWebhooks(ctx, add)
	if s.ssh != nil {
		add("ssh", "ok", "listening on %s, host key %s", s.sshLn.Addr(), s.ssh.Fingerprint())
	}
	for _, e := range s.clock.edges() {
		if since := time.Since(e.seen); since > 5*time.Minute {
			add("edge "+e.name, "warn", "last seen %s ago", since.Round(time.Second))
		} else {
			add("edge "+e.name, "ok", "following (last poll %s ago)", since.Round(time.Second))
		}
	}

	order := map[string]int{"fail": 0, "warn": 1, "ok": 2}
	sort.SliceStable(checks, func(i, j int) bool { return order[checks[i].Status] < order[checks[j].Status] })
	healthy := true
	for _, c := range checks {
		if c.Status == "fail" {
			healthy = false
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": healthy, "checks": checks})
}

func (s *Server) checkPublicURL(ctx context.Context, add func(string, string, string, ...any)) {
	u, err := url.Parse(s.cfg.PublicURL)
	if err != nil || u.Host == "" {
		add("public url", "fail", "public_url %q is not a URL", s.cfg.PublicURL)
		return
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		add("public url", "warn", "%s uses plain http: tokens and LFS links travel unencrypted; put Gitrepo behind TLS", s.cfg.PublicURL)
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(cctx, http.MethodGet, s.cfg.PublicURL+"/healthz", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		add("public url", "warn", "%s/healthz is not reachable from the server (%v); clone URLs and LFS links use public_url — check it and the reverse proxy (or the network does not allow hairpin connections)", s.cfg.PublicURL, unwrapURLErr(err))
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		add("public url", "warn", "%s/healthz answered %s: is public_url pointing at this server?", s.cfg.PublicURL, resp.Status)
		return
	}
	add("public url", "ok", "%s reaches this server", s.cfg.PublicURL)
}

func unwrapURLErr(err error) error {
	if ue, ok := err.(*url.Error); ok {
		return ue.Err
	}
	return err
}

func (s *Server) checkDisk(add func(string, string, string, ...any)) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.cfg.DataDir, &st); err != nil {
		add("disk", "warn", "cannot read free space of %s: %v", s.cfg.DataDir, err)
		return
	}
	free := st.Bavail * uint64(st.Bsize)
	total := st.Blocks * uint64(st.Bsize)
	pct := 100.0
	if total > 0 {
		pct = float64(free) * 100 / float64(total)
	}
	detail := fmt.Sprintf("%s free (%.0f%%) in %s", humanBytes(free), pct, s.cfg.DataDir)
	if free < 2<<30 || pct < 5 {
		add("disk", "warn", "%s: pushes and snapshots may fail", detail)
		return
	}
	add("disk", "ok", "%s", detail)
}

func humanBytes(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

func (s *Server) checkTarget(ctx context.Context, t *store.Target, add func(string, string, string, ...any)) {
	name := "target " + t.Name
	if t.ExpiresAt > 0 {
		exp := store.ToTime(t.ExpiresAt)
		switch left := time.Until(exp); {
		case left < 0:
			add(name+" expiry", "fail", "credential expired on %s", exp.Format("2006-01-02"))
		case left < s.cfg.Alerts.CredentialExpiryWarning.D():
			add(name+" expiry", "warn", "credential expires on %s", exp.Format("2006-01-02"))
		}
	}
	p, err := provider.Get(t.Provider)
	if err != nil {
		add(name, "fail", "%v", err)
		return
	}
	if t.Provider == "generic" {
		add(name, "ok", "%s (no API to check; mirrors show whether pushes work)", t.URLTemplate)
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	secret, err := s.targetSecret(cctx, t)
	if err != nil {
		add(name, "fail", "credential: %v", err)
		return
	}
	who, err := p.Check(cctx, t, secret)
	if err != nil {
		add(name, "fail", "%s rejected the credential: %v", t.Provider, err)
		return
	}
	add(name, "ok", "%s as %s", t.Provider, who)
}

func (s *Server) checkMirrors(ctx context.Context, add func(string, string, string, ...any)) {
	remotes, err := s.store.ListAllRemotes(ctx)
	if err != nil {
		add("mirrors", "fail", "%v", err)
		return
	}
	var failed, diverged []string
	for _, rem := range remotes {
		switch rem.State {
		case store.StateFailed:
			failed = append(failed, rem.Label())
		case store.StateDiverged:
			diverged = append(diverged, rem.Label())
		}
	}
	list := func(l []string) string {
		if len(l) > 5 {
			return strings.Join(l[:5], ", ") + " and " + strconv.Itoa(len(l)-5) + " more"
		}
		return strings.Join(l, ", ")
	}
	switch {
	case len(failed) > 0:
		add("mirrors", "warn", "%d failing: %s (gr status shows the errors)", len(failed), list(failed))
	case len(remotes) == 0:
		add("mirrors", "ok", "none yet")
	default:
		add("mirrors", "ok", "%d mirrors, none failing", len(remotes))
	}
	if len(diverged) > 0 {
		add("divergences", "warn", "%d mirror(s) have commits Gitrepo did not push: %s", len(diverged), list(diverged))
	}
}

func (s *Server) checkWebhooks(ctx context.Context, add func(string, string, string, ...any)) {
	hooks, err := s.store.ListWebhooks(ctx)
	if err != nil {
		return
	}
	for _, h := range hooks {
		if !h.Enabled || h.LastStatus == "" {
			continue
		}
		code, _ := strconv.Atoi(strings.Fields(h.LastStatus + " 0")[0])
		if strings.HasPrefix(h.LastStatus, "error") || code >= 300 {
			add("webhook "+h.Name, "warn", "last delivery failed: %s", h.LastStatus)
		}
	}
}
