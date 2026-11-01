package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/qwq-inc/gitrepo/internal/auth"
	"github.com/qwq-inc/gitrepo/internal/provider"
	"github.com/qwq-inc/gitrepo/internal/registry"
	"github.com/qwq-inc/gitrepo/internal/store"
	"github.com/qwq-inc/gitrepo/internal/syncer"
)

func (s *Server) registerAPI(mux *http.ServeMux) {
	h := func(pattern string, fn http.HandlerFunc) { mux.HandleFunc(pattern, s.requireAdmin(fn)) }

	h("GET /api/targets", s.apiListTargets)
	h("POST /api/targets", s.apiCreateTarget)
	h("GET /api/targets/{name}", s.apiGetTarget)
	h("PATCH /api/targets/{name}", s.apiUpdateTarget)
	h("DELETE /api/targets/{name}", s.apiDeleteTarget)
	h("POST /api/targets/{name}/check", s.apiCheckTarget)

	h("GET /api/bindings", s.apiListBindings)
	h("POST /api/bindings", s.apiCreateBinding)
	h("DELETE /api/bindings/{id}", s.apiDeleteBinding)

	h("GET /api/repos", s.apiListRepos)
	h("POST /api/repos", s.apiCreateRepo)
	h("GET /api/repos/{org}/{name}", s.apiGetRepo)
	h("DELETE /api/repos/{org}/{name}", s.apiDeleteRepo)
	h("POST /api/repos/{org}/{name}/remotes", s.apiAddRemote)
	h("PATCH /api/repos/{org}/{name}/remotes/{id}", s.apiUpdateRemote)
	h("DELETE /api/repos/{org}/{name}/remotes/{id}", s.apiDeleteRemote)
	h("POST /api/repos/{org}/{name}/sync", s.apiSync)
	h("GET /api/repos/{org}/{name}/runs", s.apiRuns)
	h("POST /api/repos/{org}/{name}/import", s.apiImport)

	h("GET /api/tokens", s.apiListTokens)
	h("POST /api/tokens", s.apiCreateToken)
	h("DELETE /api/tokens/{id}", s.apiDeleteToken)

	h("GET /api/status", s.apiListRepos)
	s.registerWebhookAPI(h)
	s.registerSSHAPI(h)
	s.registerBackupAPI(h)
}

func (s *Server) requireAdmin(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.auth.FromRequest(r)
		if err != nil || p == nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="gitrepo"`)
			writeErr(w, http.StatusUnauthorized, errors.New("admin token required"))
			return
		}
		if !p.IsAdmin() {
			writeErr(w, http.StatusForbidden, errors.New("admin scope required"))
			return
		}
		fn(w, r)
	}
}

// ------------------------------------------------------------------ helpers

type apiError struct {
	status int
	err    error
}

func (e *apiError) Error() string { return e.err.Error() }

func badRequest(format string, args ...any) error {
	return &apiError{status: http.StatusBadRequest, err: fmt.Errorf(format, args...)}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	var ae *apiError
	switch {
	case errors.As(err, &ae):
		writeErr(w, ae.status, ae.err)
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, store.ErrConflict):
		writeErr(w, http.StatusConflict, err)
	default:
		s.log.Error("api error", "err", err)
		writeErr(w, http.StatusInternalServerError, err)
	}
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return badRequest("invalid JSON body: %v", err)
	}
	return nil
}

func ts(ms int64) *time.Time {
	if ms == 0 {
		return nil
	}
	t := store.ToTime(ms).UTC()
	return &t
}

// ------------------------------------------------------------------ targets

type credIn struct {
	Kind          string `json:"kind"`
	Username      string `json:"username"`
	Secret        string `json:"secret"`
	SSHKnownHosts string `json:"ssh_known_hosts"`
}

type targetIn struct {
	Name        *string `json:"name"`
	Provider    *string `json:"provider"`
	BaseURL     *string `json:"base_url"`
	APIURL      *string `json:"api_url"`
	Namespace   *string `json:"namespace"`
	URLTemplate *string `json:"url_template"`
	Visibility  *string `json:"visibility"`
	AutoCreate  *bool   `json:"auto_create"`
	MaxFileSize *int64  `json:"max_file_size"`
	MaxLFSSize  *int64  `json:"max_lfs_object_size"`
	Credential  *credIn `json:"credential"`
	ExpiresAt   *string `json:"expires_at"`
}

type targetOut struct {
	ID           int64      `json:"id"`
	Name         string     `json:"name"`
	Provider     string     `json:"provider"`
	BaseURL      string     `json:"base_url,omitempty"`
	APIURL       string     `json:"api_url,omitempty"`
	Namespace    string     `json:"namespace,omitempty"`
	URLTemplate  string     `json:"url_template,omitempty"`
	Visibility   string     `json:"visibility,omitempty"`
	AutoCreate   bool       `json:"auto_create"`
	MaxFileSize  int64      `json:"max_file_size,omitempty"`
	MaxLFSSize   int64      `json:"max_lfs_object_size,omitempty"`
	CredKind     string     `json:"credential_kind"`
	CredUsername string     `json:"credential_username,omitempty"`
	HasSecret    bool       `json:"has_secret"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	CreatedAt    *time.Time `json:"created_at"`
	UpdatedAt    *time.Time `json:"updated_at"`
}

func toTargetOut(t *store.Target) targetOut {
	return targetOut{
		ID: t.ID, Name: t.Name, Provider: t.Provider, BaseURL: t.BaseURL, APIURL: t.APIURL,
		Namespace: t.Namespace, URLTemplate: t.URLTemplate, Visibility: t.Visibility, AutoCreate: t.AutoCreate,
		MaxFileSize: t.MaxFileSize, MaxLFSSize: t.MaxLFSObjectSize, CredKind: t.CredKind, CredUsername: t.CredUsername, HasSecret: len(t.CredSecret) > 0,
		ExpiresAt: ts(t.ExpiresAt), CreatedAt: ts(t.CreatedAt), UpdatedAt: ts(t.UpdatedAt),
	}
}

func (s *Server) applyTargetIn(t *store.Target, in *targetIn) error {
	set := func(dst *string, src *string) {
		if src != nil {
			*dst = strings.TrimSpace(*src)
		}
	}
	set(&t.Provider, in.Provider)
	set(&t.BaseURL, in.BaseURL)
	set(&t.APIURL, in.APIURL)
	set(&t.Namespace, in.Namespace)
	set(&t.URLTemplate, in.URLTemplate)
	set(&t.Visibility, in.Visibility)
	if in.AutoCreate != nil {
		t.AutoCreate = *in.AutoCreate
	}
	if in.MaxFileSize != nil {
		t.MaxFileSize = *in.MaxFileSize
	}
	if in.MaxLFSSize != nil {
		t.MaxLFSObjectSize = *in.MaxLFSSize
	}
	if in.ExpiresAt != nil {
		if *in.ExpiresAt == "" {
			t.ExpiresAt = 0
		} else {
			et, err := time.Parse(time.RFC3339, *in.ExpiresAt)
			if err != nil {
				if et, err = time.Parse("2006-01-02", *in.ExpiresAt); err != nil {
					return badRequest("expires_at must be RFC3339 or YYYY-MM-DD")
				}
			}
			t.ExpiresAt = store.FromTime(et)
		}
	}
	if c := in.Credential; c != nil {
		kind := c.Kind
		if kind == "" {
			kind = "token"
		}
		switch kind {
		case "none", "token", "basic", "ssh":
		default:
			return badRequest("credential kind must be none, token, basic or ssh")
		}
		// An omitted username keeps the stored one (unless the kind changes).
		if c.Username != "" || kind != t.CredKind {
			t.CredUsername = c.Username
		}
		t.CredKind = kind
		if c.SSHKnownHosts != "" || kind != "ssh" {
			t.SSHKnownHosts = c.SSHKnownHosts
		}
		if kind == "none" {
			t.CredSecret = nil
		} else if c.Secret != "" {
			sealed, err := s.box.Seal([]byte(c.Secret), t.CredentialAAD())
			if err != nil {
				return err
			}
			t.CredSecret = sealed
		} else if len(t.CredSecret) == 0 {
			return badRequest("credential secret is required for kind %s", kind)
		}
	}
	switch t.Visibility {
	case "", "private", "public", "internal":
	default:
		return badRequest("visibility must be private, public or internal")
	}
	for field, v := range map[string]string{"base_url": t.BaseURL, "api_url": t.APIURL} {
		if v == "" {
			continue
		}
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.User != nil {
			return badRequest("%s must be an http(s) URL without credentials or query, e.g. https://git.example.com", field)
		}
		if _, err := strconv.Atoi(u.Port()); u.Port() != "" && err != nil {
			return badRequest("%s has an invalid port", field)
		}
	}
	p, err := provider.Get(t.Provider)
	if err != nil {
		return badRequest("%v", err)
	}
	if err := p.Validate(t); err != nil {
		return badRequest("%v", err)
	}
	return nil
}

func (s *Server) apiListTargets(w http.ResponseWriter, r *http.Request) {
	ts, err := s.store.ListTargets(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []targetOut{}
	for _, t := range ts {
		out = append(out, toTargetOut(t))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiCreateTarget(w http.ResponseWriter, r *http.Request) {
	var in targetIn
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	if in.Name == nil || !validName(*in.Name) {
		s.fail(w, badRequest("name is required (letters, digits, '-', '_', '.')"))
		return
	}
	t := &store.Target{Name: *in.Name, CredKind: "none", AutoCreate: true}
	if err := s.applyTargetIn(t, &in); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.fillLoginUsername(r.Context(), t, &in); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.CreateTarget(r.Context(), t); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toTargetOut(t))
}

func validName(n string) bool {
	if n == "" || len(n) > 64 {
		return false
	}
	for _, c := range n {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func (s *Server) apiGetTarget(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.GetTargetByName(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTargetOut(t))
}

func (s *Server) apiUpdateTarget(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.GetTargetByName(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	var in targetIn
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	if in.Name != nil && *in.Name != t.Name {
		s.fail(w, badRequest("targets cannot be renamed"))
		return
	}
	if err := s.applyTargetIn(t, &in); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.fillLoginUsername(r.Context(), t, &in); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.UpdateTarget(r.Context(), t); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTargetOut(t))
}

func (s *Server) apiDeleteTarget(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.GetTargetByName(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.DeleteTarget(r.Context(), t.ID); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			err = &apiError{status: http.StatusConflict, err: err}
		}
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiCheckTarget(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.GetTargetByName(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	p, err := provider.Get(t.Provider)
	if err != nil {
		s.fail(w, err)
		return
	}
	secret, err := s.targetSecret(t)
	if err != nil {
		s.fail(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	who, err := p.Check(ctx, t, secret)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "identity": who})
}

// fillLoginUsername asks the platform for the token owner's login when the
// provider needs it as the Git username and none was given.
func (s *Server) fillLoginUsername(ctx context.Context, t *store.Target, in *targetIn) error {
	p, err := provider.Get(t.Provider)
	if err != nil {
		return err
	}
	lu, ok := p.(provider.LoginUsername)
	if !ok || !lu.UsesLoginUsername() || in.Credential == nil || in.Credential.Secret == "" || in.Credential.Username != "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	login, err := p.Check(ctx, t, in.Credential.Secret)
	if err != nil {
		return badRequest("cannot look up the token owner's login (pass --username): %v", err)
	}
	t.CredUsername = login
	return nil
}

func (s *Server) targetSecret(t *store.Target) (string, error) {
	if len(t.CredSecret) == 0 {
		return "", nil
	}
	b, err := s.box.Open(t.CredSecret, t.CredentialAAD())
	return string(b), err
}

// ----------------------------------------------------------------- bindings

type bindingOut struct {
	ID           int64  `json:"id"`
	Org          string `json:"org"`
	Target       string `json:"target"`
	Namespace    string `json:"namespace,omitempty"`
	OnDivergence string `json:"on_divergence"`
	NameTemplate string `json:"name_template"`
}

func (s *Server) apiListBindings(w http.ResponseWriter, r *http.Request) {
	bs, err := s.store.ListBindings(r.Context(), "")
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []bindingOut{}
	for _, b := range bs {
		out = append(out, bindingOut{ID: b.ID, Org: b.Org, Target: b.TargetName, Namespace: b.Namespace, OnDivergence: b.OnDivergence, NameTemplate: b.NameTemplate})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiCreateBinding(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Org           string `json:"org"`
		Target        string `json:"target"`
		Namespace     string `json:"namespace"`
		OnDivergence  string `json:"on_divergence"`
		NameTemplate  string `json:"name_template"`
		ApplyExisting bool   `json:"apply_existing"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	if in.Org == "" || in.Target == "" {
		s.fail(w, badRequest("org and target are required"))
		return
	}
	if in.Org != "*" && !validName(in.Org) {
		s.fail(w, badRequest("invalid org %q", in.Org))
		return
	}
	if in.OnDivergence == "" {
		in.OnDivergence = store.OnDivergenceAlert
	}
	if in.OnDivergence != store.OnDivergenceAlert && in.OnDivergence != store.OnDivergenceOverwrite {
		s.fail(w, badRequest("on_divergence must be alert or overwrite"))
		return
	}
	if in.NameTemplate == "" {
		in.NameTemplate = "{name}"
	}
	if !strings.Contains(in.NameTemplate, "{name}") || strings.ContainsAny(strings.NewReplacer("{org}", "", "{name}", "").Replace(in.NameTemplate), "/{} ") {
		s.fail(w, badRequest("name_template must contain {name} and may add {org} and literal text, e.g. {org}-{name}"))
		return
	}
	t, err := s.store.GetTargetByName(r.Context(), in.Target)
	if err != nil {
		s.fail(w, badRequest("target %q not found", in.Target))
		return
	}
	b := &store.Binding{Org: in.Org, TargetID: t.ID, TargetName: t.Name, Namespace: in.Namespace, OnDivergence: in.OnDivergence, NameTemplate: in.NameTemplate}
	if err := s.store.CreateBinding(r.Context(), b); err != nil {
		s.fail(w, err)
		return
	}
	applied := 0
	skipped := []string{}
	if in.ApplyExisting {
		repos, err := s.store.ListRepos(r.Context())
		if err != nil {
			s.fail(w, err)
			return
		}
		for _, repo := range repos {
			org, _, _ := strings.Cut(repo.Slug, "/")
			if in.Org != "*" && in.Org != org {
				continue
			}
			// A non-empty pre-existing mirror still has to be adopted.
			rem, err := s.reg.ApplyBinding(r.Context(), repo, b)
			switch {
			case err == nil:
				applied++
				s.mgr.Enqueue(repo.ID, syncer.Request{Trigger: syncer.TriggerManual, RemoteIDs: []int64{rem.ID}})
			case errors.Is(err, registry.ErrLocationTaken):
				skipped = append(skipped, repo.Slug+": "+err.Error())
			case errors.Is(err, store.ErrConflict):
				// already mirrored to this target
			default:
				s.fail(w, err)
				return
			}
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"binding":          bindingOut{ID: b.ID, Org: b.Org, Target: t.Name, Namespace: b.Namespace, OnDivergence: b.OnDivergence, NameTemplate: b.NameTemplate},
		"applied_to_repos": applied,
		"skipped":          skipped,
	})
}

func (s *Server) apiDeleteBinding(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.fail(w, badRequest("invalid id"))
		return
	}
	if err := s.store.DeleteBinding(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// -------------------------------------------------------------------- repos

type divOut struct {
	Ref         string     `json:"ref"`
	HubSHA      string     `json:"hub_sha,omitempty"`
	RemoteSHA   string     `json:"remote_sha,omitempty"`
	ExpectedSHA string     `json:"expected_sha,omitempty"`
	Reason      string     `json:"reason"`
	DetectedAt  *time.Time `json:"detected_at"`
}

type remoteOut struct {
	ID                  int64      `json:"id"`
	Label               string     `json:"label"`
	Target              string     `json:"target"`
	Provider            string     `json:"provider"`
	Namespace           string     `json:"namespace,omitempty"`
	Name                string     `json:"name"`
	URL                 string     `json:"url"`
	OnDivergence        string     `json:"on_divergence"`
	Enabled             bool       `json:"enabled"`
	Adopt               bool       `json:"adopt"`
	NeedsAdoption       bool       `json:"needs_adoption,omitempty"`
	State               string     `json:"state"`
	LastError           string     `json:"last_error,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures,omitempty"`
	LastSyncAt          *time.Time `json:"last_sync_at,omitempty"`
	LastSuccessAt       *time.Time `json:"last_success_at,omitempty"`
	NextRetryAt         *time.Time `json:"next_retry_at,omitempty"`
	Divergences         []divOut   `json:"divergences,omitempty"`
}

type repoOut struct {
	ID            int64       `json:"id"`
	Slug          string      `json:"slug"`
	CloneURL      string      `json:"clone_url"`
	SSHURL        string      `json:"ssh_url,omitempty"`
	DefaultBranch string      `json:"default_branch,omitempty"`
	Visibility    string      `json:"visibility"`
	State         string      `json:"state"`
	Busy          bool        `json:"busy"`
	CreatedAt     *time.Time  `json:"created_at"`
	Remotes       []remoteOut `json:"remotes"`
}

// stateRank orders remote states from best to worst for repo aggregation.
var stateRank = map[string]int{
	store.StateSynced: 0, store.StatePaused: 1, store.StatePending: 2, store.StateSyncing: 3,
	store.StateBehind: 4, store.StateDiverged: 5, store.StateFailed: 6,
}

func (s *Server) repoOut(ctx context.Context, repo *store.Repo, withDivergences bool) (repoOut, error) {
	out := repoOut{
		ID: repo.ID, Slug: repo.Slug, CloneURL: s.cfg.PublicURL + "/" + repo.Slug + ".git", SSHURL: s.SSHCloneURL(repo.Slug),
		DefaultBranch: repo.DefaultBranch, Visibility: repo.Visibility, CreatedAt: ts(repo.CreatedAt),
		Busy: s.mgr.Busy(repo.ID), Remotes: []remoteOut{},
	}
	remotes, err := s.store.ListRemotes(ctx, repo.ID)
	if err != nil {
		return out, err
	}
	out.State = "no-remotes"
	worst := -1
	targets := map[int64]*store.Target{}
	for _, rem := range remotes {
		t := targets[rem.TargetID]
		if t == nil {
			if t, err = s.store.GetTarget(ctx, rem.TargetID); err != nil {
				return out, err
			}
			targets[rem.TargetID] = t
		}
		url := rem.URL
		if url == "" {
			if p, err := provider.Get(t.Provider); err == nil {
				url, _ = p.RepoURL(t, rem.Namespace, rem.Name)
			}
		}
		ro := remoteOut{
			ID: rem.ID, Label: rem.Label(), Target: rem.TargetName, Provider: rem.Provider, Namespace: rem.EffectiveNamespace(),
			Name: rem.Name, URL: url, OnDivergence: rem.OnDivergence, Enabled: rem.Enabled, State: rem.State,
			Adopt: rem.Adopt, NeedsAdoption: !rem.Adopt && strings.HasPrefix(rem.LastError, syncer.ErrNeedsAdoption.Error()),
			LastError: rem.LastError, ConsecutiveFailures: rem.ConsecutiveFailures,
			LastSyncAt: ts(rem.LastSyncAt), LastSuccessAt: ts(rem.LastSuccessAt), NextRetryAt: ts(rem.NextRetryAt),
		}
		if withDivergences {
			divs, err := s.store.ListDivergences(ctx, rem.ID)
			if err != nil {
				return out, err
			}
			for _, d := range divs {
				ro.Divergences = append(ro.Divergences, divOut{Ref: d.Ref, HubSHA: d.HubSHA, RemoteSHA: d.RemoteSHA,
					ExpectedSHA: d.ExpectedSHA, Reason: d.Reason, DetectedAt: ts(d.DetectedAt)})
			}
		}
		if r := stateRank[rem.State]; r > worst {
			worst = r
			out.State = rem.State
		}
		out.Remotes = append(out.Remotes, ro)
	}
	return out, nil
}

func (s *Server) repoFromPath(r *http.Request) (*store.Repo, error) {
	slug := r.PathValue("org") + "/" + strings.TrimSuffix(r.PathValue("name"), ".git")
	repo, err := s.store.GetRepoBySlug(r.Context(), slug)
	if errors.Is(err, store.ErrNotFound) {
		return nil, &apiError{status: http.StatusNotFound, err: fmt.Errorf("repository %s not found", slug)}
	}
	return repo, err
}

func (s *Server) apiListRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := s.store.ListRepos(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []repoOut{}
	for _, repo := range repos {
		ro, err := s.repoOut(r.Context(), repo, false)
		if err != nil {
			s.fail(w, err)
			return
		}
		out = append(out, ro)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiCreateRepo(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Slug       string   `json:"slug"`
		Visibility string   `json:"visibility"`
		Bindings   *bool    `json:"bindings"`
		Targets    []string `json:"targets"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	useBindings := in.Bindings == nil || *in.Bindings
	repo, created, err := s.reg.CreateRepo(r.Context(), in.Slug, in.Visibility, useBindings)
	if err != nil {
		if !errors.Is(err, store.ErrConflict) {
			err = badRequest("%v", err)
		}
		s.fail(w, err)
		return
	}
	if !created {
		s.fail(w, &apiError{status: http.StatusConflict, err: fmt.Errorf("repository %s already exists", repo.Slug)})
		return
	}
	for _, t := range in.Targets {
		if _, err := s.reg.AddRemote(r.Context(), repo, t, "", "", "", "", false); err != nil {
			s.fail(w, badRequest("add remote %s: %v", t, err))
			return
		}
	}
	ro, err := s.repoOut(r.Context(), repo, false)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ro)
}

func (s *Server) apiGetRepo(w http.ResponseWriter, r *http.Request) {
	repo, err := s.repoFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	ro, err := s.repoOut(r.Context(), repo, true)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ro)
}

func (s *Server) apiDeleteRepo(w http.ResponseWriter, r *http.Request) {
	repo, err := s.repoFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	if s.mgr.Busy(repo.ID) {
		s.fail(w, &apiError{status: http.StatusConflict, err: errors.New("a sync is running; try again shortly")})
		return
	}
	trash, err := s.reg.DeleteRepo(r.Context(), repo)
	if err != nil {
		s.fail(w, err)
		return
	}
	if s.bundles != nil {
		s.bundles.Remove(repo.Slug)
	}
	writeJSON(w, http.StatusOK, map[string]string{"moved_to": trash})
}

func (s *Server) apiAddRemote(w http.ResponseWriter, r *http.Request) {
	repo, err := s.repoFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	var in struct {
		Target       string `json:"target"`
		Namespace    string `json:"namespace"`
		Name         string `json:"name"`
		URL          string `json:"url"`
		OnDivergence string `json:"on_divergence"`
		Adopt        bool   `json:"adopt"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	rem, err := s.reg.AddRemote(r.Context(), repo, in.Target, in.Namespace, in.Name, in.URL, in.OnDivergence, in.Adopt)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, registry.ErrLocationTaken) {
			status = http.StatusConflict
		}
		s.fail(w, &apiError{status: status, err: err})
		return
	}
	s.mgr.Enqueue(repo.ID, syncer.Request{Trigger: syncer.TriggerManual, RemoteIDs: []int64{rem.ID}})
	ro, err := s.repoOut(r.Context(), repo, false)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ro)
}

func (s *Server) remoteFromPath(r *http.Request, repo *store.Repo) (*store.Remote, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil, badRequest("invalid remote id")
	}
	rem, err := s.store.GetRemote(r.Context(), id)
	if err != nil || rem.RepoID != repo.ID {
		return nil, &apiError{status: http.StatusNotFound, err: errors.New("remote not found")}
	}
	return rem, nil
}

func (s *Server) apiUpdateRemote(w http.ResponseWriter, r *http.Request) {
	repo, err := s.repoFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	rem, err := s.remoteFromPath(r, repo)
	if err != nil {
		s.fail(w, err)
		return
	}
	var in struct {
		Enabled      *bool   `json:"enabled"`
		OnDivergence *string `json:"on_divergence"`
		URL          *string `json:"url"`
		Adopt        *bool   `json:"adopt"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	if in.Enabled != nil {
		rem.Enabled = *in.Enabled
	}
	if in.OnDivergence != nil {
		if *in.OnDivergence != store.OnDivergenceAlert && *in.OnDivergence != store.OnDivergenceOverwrite {
			s.fail(w, badRequest("on_divergence must be alert or overwrite"))
			return
		}
		rem.OnDivergence = *in.OnDivergence
	}
	if in.URL != nil {
		rem.URL = strings.TrimSpace(*in.URL)
		if err := s.reg.LocationConflict(r.Context(), repo.ID, rem.TargetID, rem.Namespace, rem.Name, rem.URL); err != nil {
			s.fail(w, &apiError{status: http.StatusConflict, err: err})
			return
		}
	}
	if in.Adopt != nil {
		rem.Adopt = *in.Adopt
	}
	if err := s.store.UpdateRemoteConfig(r.Context(), rem); err != nil {
		s.fail(w, err)
		return
	}
	s.mgr.Enqueue(repo.ID, syncer.Request{Trigger: syncer.TriggerManual, RemoteIDs: []int64{rem.ID}})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiDeleteRemote(w http.ResponseWriter, r *http.Request) {
	repo, err := s.repoFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	rem, err := s.remoteFromPath(r, repo)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.DeleteRemote(r.Context(), rem.ID); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiSync(w http.ResponseWriter, r *http.Request) {
	repo, err := s.repoFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	var in struct {
		Overwrite bool    `json:"overwrite"`
		Wait      bool    `json:"wait"`
		RemoteIDs []int64 `json:"remote_ids"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	events := s.mgr.Enqueue(repo.ID, syncer.Request{Trigger: syncer.TriggerManual, Overwrite: in.Overwrite, RemoteIDs: in.RemoteIDs})
	if !in.Wait {
		writeJSON(w, http.StatusAccepted, map[string]any{"queued": true})
		return
	}
	timeout := time.NewTimer(s.cfg.Sync.PushTimeout.D() + time.Minute)
	defer timeout.Stop()
	var got []syncer.Event
	for {
		select {
		case e, ok := <-events:
			if !ok {
				writeJSON(w, http.StatusOK, map[string]any{"events": got})
				return
			}
			got = append(got, e)
		case <-timeout.C:
			writeJSON(w, http.StatusOK, map[string]any{"events": got, "timeout": true})
			return
		case <-r.Context().Done():
			return
		}
	}
}

type runOut struct {
	ID          int64      `json:"id"`
	Remote      string     `json:"remote"`
	Trigger     string     `json:"trigger"`
	Status      string     `json:"status"`
	RefsChanged int        `json:"refs_changed"`
	Error       string     `json:"error,omitempty"`
	Detail      string     `json:"detail,omitempty"`
	StartedAt   *time.Time `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

func (s *Server) apiRuns(w http.ResponseWriter, r *http.Request) {
	repo, err := s.repoFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	runs, err := s.store.ListRuns(r.Context(), repo.ID, limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []runOut{}
	for _, run := range runs {
		out = append(out, runOut{ID: run.ID, Remote: run.RemoteLabel, Trigger: run.Trigger, Status: run.Status,
			RefsChanged: run.RefsChanged, Error: run.Error, Detail: run.Detail, StartedAt: ts(run.StartedAt), FinishedAt: ts(run.FinishedAt)})
	}
	writeJSON(w, http.StatusOK, out)
}

// ------------------------------------------------------------------- tokens

type tokenOut struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	Repos      []string   `json:"repos,omitempty"`
	Token      string     `json:"token,omitempty"`
	CreatedAt  *time.Time `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

func (s *Server) apiListTokens(w http.ResponseWriter, r *http.Request) {
	toks, err := s.store.ListTokens(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []tokenOut{}
	for _, t := range toks {
		out = append(out, tokenOut{ID: t.ID, Name: t.Name, Scopes: t.Scopes, Repos: t.Repos, CreatedAt: ts(t.CreatedAt), LastUsedAt: ts(t.LastUsedAt)})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiCreateToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
		Repos  []string `json:"repos"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	if !validName(in.Name) {
		s.fail(w, badRequest("name is required (letters, digits, '-', '_', '.')"))
		return
	}
	scopes, err := auth.ParseScopes(strings.Join(in.Scopes, ","))
	if err != nil {
		s.fail(w, badRequest("%v", err))
		return
	}
	repos, err := auth.ParseRepoPatterns(strings.Join(in.Repos, ","))
	if err != nil {
		s.fail(w, badRequest("%v", err))
		return
	}
	plain, hash := auth.NewToken()
	t := &store.Token{Name: in.Name, Hash: hash, Scopes: scopes, Repos: repos}
	if err := s.store.CreateToken(r.Context(), t); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, tokenOut{ID: t.ID, Name: t.Name, Scopes: t.Scopes, Repos: t.Repos, Token: plain, CreatedAt: ts(t.CreatedAt)})
}

func (s *Server) apiDeleteToken(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.fail(w, badRequest("invalid id"))
		return
	}
	if err := s.store.DeleteToken(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
