package server

import (
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/qwq-inc/gitrepo/internal/auth"
	"github.com/qwq-inc/gitrepo/internal/githooks"
	"github.com/qwq-inc/gitrepo/internal/store"
)

// Branch protection: rules that keep branches (or tags) from being
// force-pushed or deleted on the hub. Enforced by the pre-receive hook, for
// HTTPS and SSH pushes alike; mirrors only ever receive what the hub has.

type protectionOut struct {
	ID         int64      `json:"id"`
	Repos      string     `json:"repos"`
	Ref        string     `json:"ref"`
	DenyForce  bool       `json:"deny_force"`
	DenyDelete bool       `json:"deny_delete"`
	CreatedAt  *time.Time `json:"created_at"`
}

func toProtectionOut(p *store.Protection) protectionOut {
	return protectionOut{ID: p.ID, Repos: p.Repos, Ref: p.Ref, DenyForce: p.DenyForce, DenyDelete: p.DenyDelete, CreatedAt: ts(p.CreatedAt)}
}

func (s *Server) registerProtectAPI(h func(string, http.HandlerFunc)) {
	h("GET /api/protections", s.apiListProtections)
	h("POST /api/protections", s.apiCreateProtection)
	h("DELETE /api/protections/{id}", s.apiDeleteProtection)
}

func (s *Server) apiListProtections(w http.ResponseWriter, r *http.Request) {
	ps, err := s.store.ListProtections(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []protectionOut{}
	for _, p := range ps {
		out = append(out, toProtectionOut(p))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiCreateProtection(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Repos      string `json:"repos"`
		Ref        string `json:"ref"`
		DenyForce  *bool  `json:"deny_force"`
		DenyDelete *bool  `json:"deny_delete"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	in.Repos, in.Ref = strings.TrimSpace(in.Repos), strings.TrimSpace(in.Ref)
	if in.Repos == "" {
		in.Repos = "*"
	}
	if _, err := path.Match(in.Repos, "a/b"); err != nil || strings.ContainsAny(in.Repos, " \t\n") {
		s.fail(w, badRequest("repos must be a repository or pattern, e.g. acme/app, acme/* or *"))
		return
	}
	if err := githooks.ValidateRefPattern(in.Ref); err != nil {
		s.fail(w, badRequest("%v", err))
		return
	}
	p := &store.Protection{Repos: in.Repos, Ref: in.Ref, DenyForce: true, DenyDelete: true}
	if in.DenyForce != nil {
		p.DenyForce = *in.DenyForce
	}
	if in.DenyDelete != nil {
		p.DenyDelete = *in.DenyDelete
	}
	if !p.DenyForce && !p.DenyDelete {
		s.fail(w, badRequest("a rule must refuse force-pushes, deletion or both"))
		return
	}
	if err := s.store.CreateProtection(r.Context(), p); err != nil {
		if err == store.ErrConflict {
			err = &apiError{status: http.StatusConflict, err: fmt.Errorf("a rule for %s %s already exists; remove it first", p.Repos, p.Ref)}
		}
		s.fail(w, err)
		return
	}
	noteAudit(r, "protect %s in %s (%s)", p.Ref, p.Repos, describeProtection(p))
	writeJSON(w, http.StatusCreated, toProtectionOut(p))
}

func (s *Server) apiDeleteProtection(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.fail(w, badRequest("invalid id"))
		return
	}
	p, err := s.store.DeleteProtection(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	noteAudit(r, "unprotect %s in %s (was: %s)", p.Ref, p.Repos, describeProtection(p))
	w.WriteHeader(http.StatusNoContent)
}

func describeProtection(p *store.Protection) string {
	var parts []string
	if p.DenyForce {
		parts = append(parts, "no force-push")
	}
	if p.DenyDelete {
		parts = append(parts, "no deletion")
	}
	return strings.Join(parts, ", ")
}

// protectionsFor lists the rules that apply to slug (repository page).
func (s *Server) protectionsFor(r *http.Request, slug string) []protectionOut {
	ps, err := s.store.ListProtections(r.Context())
	if err != nil {
		return nil
	}
	var out []protectionOut
	for _, p := range ps {
		if auth.MatchRepo([]string{p.Repos}, slug) {
			out = append(out, toProtectionOut(p))
		}
	}
	return out
}
