package server

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/qwq-inc/gitrepo/internal/deploy"
	"github.com/qwq-inc/gitrepo/internal/store"
)

func (s *Server) registerDeployAPI(h func(string, http.HandlerFunc)) {
	h("GET /api/repos/{org}/{name}/deploy-hooks", s.apiListDeployHooks)
	h("POST /api/repos/{org}/{name}/deploy-hooks", s.apiCreateDeployHook)
	h("PATCH /api/repos/{org}/{name}/deploy-hooks/{id}", s.apiUpdateDeployHook)
	h("DELETE /api/repos/{org}/{name}/deploy-hooks/{id}", s.apiDeleteDeployHook)
	h("POST /api/repos/{org}/{name}/deploy-hooks/{id}/trigger", s.apiTriggerDeployHook)
}

// resolveDeployRef returns a branch's current commit on the hub, or on a
// mirror as far as Gitrepo has pushed it.
func (s *Server) resolveDeployRef(ctx context.Context, repo *store.Repo, remoteID int64, ref string) (string, error) {
	if remoteID == 0 {
		refs, err := s.git.ForEachRef(ctx, s.reg.Path(repo.Slug), ref)
		if err != nil {
			return "", err
		}
		return refs[ref], nil
	}
	pushed, err := s.store.GetPushedState(ctx, remoteID)
	if err != nil {
		return "", err
	}
	return pushed[ref], nil
}

type deployHookOut struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	Host       string     `json:"host"`
	Branch     string     `json:"branch"`
	BranchSet  bool       `json:"branch_set"`
	RemoteID   int64      `json:"remote_id,omitempty"`
	Source     string     `json:"source"`
	Signed     bool       `json:"signed"`
	Token      bool       `json:"token"`
	Enabled    bool       `json:"enabled"`
	LastStatus string     `json:"last_status,omitempty"`
	LastCommit string     `json:"last_commit,omitempty"`
	LastAt     *time.Time `json:"last_at,omitempty"`
}

func (s *Server) toDeployOut(ctx context.Context, repo *store.Repo, h *store.DeployHook) deployHookOut {
	out := deployHookOut{
		ID: h.ID, Name: h.Name, Kind: h.Kind, Host: h.URLHost, BranchSet: h.Branch != "", RemoteID: h.RemoteID,
		Source: "hub", Signed: len(h.Secret) > 0, Token: len(h.Token) > 0, Enabled: h.Enabled,
		LastStatus: h.LastStatus, LastCommit: h.LastCommit, LastAt: ts(h.LastAt),
	}
	var rem *store.Remote
	if h.RemoteID != 0 {
		if r, err := s.store.GetRemote(ctx, h.RemoteID); err == nil {
			rem = r
			out.Source = r.Label()
		}
	}
	out.Branch = deploy.Branch(h, repo, rem)
	return out
}

func (s *Server) apiListDeployHooks(w http.ResponseWriter, r *http.Request) {
	repo, err := s.repoFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	hooks, err := s.store.ListDeployHooks(r.Context(), repo.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []deployHookOut{}
	for _, h := range hooks {
		out = append(out, s.toDeployOut(r.Context(), repo, h))
	}
	writeJSON(w, http.StatusOK, out)
}

// deployIn is the body of create and update. On update, absent fields keep
// their value; an empty secret or token keeps the stored one (clear_secret
// and clear_token remove them). source is "hub", or a mirror's id or label.
type deployIn struct {
	Name        string  `json:"name"`
	Kind        *string `json:"kind"`
	URL         *string `json:"url"`
	Secret      *string `json:"secret"`
	ClearSecret bool    `json:"clear_secret"`
	Token       *string `json:"token"`
	ClearToken  bool    `json:"clear_token"`
	Branch      *string `json:"branch"`
	Source      *string `json:"source"`
	Enabled     *bool   `json:"enabled"`
}

func (s *Server) applyDeployIn(ctx context.Context, repo *store.Repo, h *store.DeployHook, in *deployIn) error {
	if in.Kind != nil {
		h.Kind = strings.TrimSpace(*in.Kind)
	}
	if h.Kind == "" {
		h.Kind = deploy.KindGeneric
	}
	if !slices.Contains(deploy.Kinds, h.Kind) {
		return badRequest("kind must be one of %s", strings.Join(deploy.Kinds, ", "))
	}
	if in.URL != nil {
		u, err := deploy.CheckURL(*in.URL)
		if err != nil {
			return badRequest("%v", err)
		}
		if h.URL, err = s.box.Seal([]byte(u.String()), h.AAD("url")); err != nil {
			return err
		}
		h.URLHost = u.Host
	}
	if len(h.URL) == 0 {
		return badRequest("url is required")
	}
	switch {
	case in.Secret != nil && *in.Secret != "":
		if _, err := deploy.SecretKey(*in.Secret); err != nil {
			return badRequest("secret: %v", err)
		}
		sealed, err := s.box.Seal([]byte(*in.Secret), h.AAD("secret"))
		if err != nil {
			return err
		}
		h.Secret = sealed
	case in.ClearSecret:
		h.Secret = nil
	}
	switch {
	case in.Token != nil && *in.Token != "":
		sealed, err := s.box.Seal([]byte(strings.TrimSpace(*in.Token)), h.AAD("token"))
		if err != nil {
			return err
		}
		h.Token = sealed
	case in.ClearToken:
		h.Token = nil
	}
	if in.Branch != nil {
		b := strings.TrimPrefix(strings.TrimSpace(*in.Branch), "refs/heads/")
		if b != "" && !validRefName(b) {
			return badRequest("invalid branch %q", b)
		}
		h.Branch = b
	}
	if in.Source != nil {
		id, err := s.deploySource(ctx, repo, strings.TrimSpace(*in.Source))
		if err != nil {
			return err
		}
		h.RemoteID = id
	}
	if in.Enabled != nil {
		h.Enabled = *in.Enabled
	}
	return nil
}

func validRefName(b string) bool {
	return !strings.ContainsAny(b, " \t\n~^:?*[\\") && !strings.Contains(b, "..") && !strings.HasPrefix(b, "-") &&
		!strings.HasPrefix(b, "/") && !strings.HasSuffix(b, "/") && !strings.HasSuffix(b, ".lock") && !strings.Contains(b, "//")
}

// deploySource resolves "hub" (or "") to 0 and a mirror's id or label to
// its id.
func (s *Server) deploySource(ctx context.Context, repo *store.Repo, src string) (int64, error) {
	if src == "" || src == "hub" {
		return 0, nil
	}
	remotes, err := s.store.ListRemotes(ctx, repo.ID)
	if err != nil {
		return 0, err
	}
	for _, rem := range remotes {
		if strconv.FormatInt(rem.ID, 10) == src || rem.Label() == src {
			return rem.ID, nil
		}
	}
	return 0, badRequest("source %q is neither \"hub\" nor a mirror of %s", src, repo.Slug)
}

func (s *Server) apiCreateDeployHook(w http.ResponseWriter, r *http.Request) {
	repo, err := s.repoFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	var in deployIn
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	if !validName(in.Name) {
		s.fail(w, badRequest("name is required (letters, digits, '-', '_', '.')"))
		return
	}
	h := &store.DeployHook{RepoID: repo.ID, Name: in.Name, Enabled: true}
	if err := s.applyDeployIn(r.Context(), repo, h, &in); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.CreateDeployHook(r.Context(), h); err != nil {
		if errors.Is(err, store.ErrConflict) {
			err = &apiError{status: http.StatusConflict, err: errors.New("this repository already has a deploy hook with that name")}
		}
		s.fail(w, err)
		return
	}
	out := s.toDeployOut(r.Context(), repo, h)
	noteAuditRepo(r, repo.Slug)
	noteAudit(r, "deploy hook %s → %s (%s on %s)", h.Name, h.URLHost, out.Branch, out.Source)
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) deployHookFromPath(r *http.Request, repo *store.Repo) (*store.DeployHook, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil, badRequest("invalid id")
	}
	h, err := s.store.GetDeployHook(r.Context(), id)
	if err != nil || h.RepoID != repo.ID {
		return nil, &apiError{status: http.StatusNotFound, err: errors.New("deploy hook not found")}
	}
	return h, nil
}

func (s *Server) apiUpdateDeployHook(w http.ResponseWriter, r *http.Request) {
	repo, err := s.repoFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	h, err := s.deployHookFromPath(r, repo)
	if err != nil {
		s.fail(w, err)
		return
	}
	var in deployIn
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	if in.Name != "" && in.Name != h.Name {
		s.fail(w, badRequest("a deploy hook cannot be renamed"))
		return
	}
	if err := s.applyDeployIn(r.Context(), repo, h, &in); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.UpdateDeployHook(r.Context(), h); err != nil {
		s.fail(w, err)
		return
	}
	out := s.toDeployOut(r.Context(), repo, h)
	noteAuditRepo(r, repo.Slug)
	noteAudit(r, "deploy hook %s → %s (%s on %s), enabled %t", h.Name, h.URLHost, out.Branch, out.Source, h.Enabled)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiDeleteDeployHook(w http.ResponseWriter, r *http.Request) {
	repo, err := s.repoFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	h, err := s.deployHookFromPath(r, repo)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.DeleteDeployHook(r.Context(), h.ID); err != nil {
		s.fail(w, err)
		return
	}
	noteAuditRepo(r, repo.Slug)
	noteAudit(r, "deploy hook %s → %s", h.Name, h.URLHost)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiTriggerDeployHook(w http.ResponseWriter, r *http.Request) {
	repo, err := s.repoFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	h, err := s.deployHookFromPath(r, repo)
	if err != nil {
		s.fail(w, err)
		return
	}
	commit, status, err := s.deploy.Trigger(r.Context(), repo, h)
	noteAuditRepo(r, repo.Slug)
	if commit == "" && err != nil {
		s.fail(w, badRequest("%v", err))
		return
	}
	noteAudit(r, "deploy hook %s: %.10s → %s", h.Name, commit, status)
	out := map[string]any{"commit": commit, "status": status, "ok": err == nil}
	if err != nil {
		out["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, out)
}
