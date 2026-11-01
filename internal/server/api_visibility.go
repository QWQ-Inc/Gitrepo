package server

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/qwq-inc/gitrepo/internal/provider"
	"github.com/qwq-inc/gitrepo/internal/store"
)

// PATCH /api/repos/{org}/{name} changes a repository's visibility on the
// hub and then on every mirror whose target follows the repository (a
// target with a fixed visibility keeps it). The hub changes first, so a
// restriction takes effect there at once; each mirror's outcome is
// reported, because a mirror left public after the hub went private must
// not go unnoticed.

type visibilityResult struct {
	Mirror string `json:"mirror"`
	State  string `json:"state"` // changed | kept | unsupported | failed
	Detail string `json:"detail,omitempty"`
}

func (s *Server) apiUpdateRepo(w http.ResponseWriter, r *http.Request) {
	repo, err := s.repoFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	var in struct {
		Visibility *string `json:"visibility"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	if in.Visibility == nil {
		s.fail(w, badRequest("nothing to change (visibility)"))
		return
	}
	vis := *in.Visibility
	if vis != "private" && vis != "public" && vis != "internal" {
		s.fail(w, badRequest("visibility must be private, internal or public"))
		return
	}
	old := repo.Visibility
	results := []visibilityResult{}
	if vis != old {
		repo.Visibility = vis
		if err := s.store.UpdateRepo(r.Context(), repo); err != nil {
			s.fail(w, err)
			return
		}
		results = s.setMirrorVisibility(r.Context(), repo, vis)
	}
	changed, failed := 0, 0
	for _, res := range results {
		switch res.State {
		case "changed":
			changed++
		case "failed", "unsupported":
			failed++
		}
	}
	noteAudit(r, "visibility %s → %s (mirrors: %d changed, %d not changed)", old, vis, changed, failed)
	ro, err := s.repoOut(r.Context(), repo, false)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repo": ro, "mirrors": results})
}

func (s *Server) setMirrorVisibility(parent context.Context, repo *store.Repo, vis string) []visibilityResult {
	remotes, err := s.store.ListRemotes(parent, repo.ID)
	if err != nil {
		return []visibilityResult{{Mirror: "(all)", State: "failed", Detail: err.Error()}}
	}
	results := make([]visibilityResult, len(remotes))
	var wg sync.WaitGroup
	for i, rem := range remotes {
		wg.Add(1)
		go func(i int, rem *store.Remote) {
			defer wg.Done()
			res := visibilityResult{Mirror: rem.Label()}
			defer func() { results[i] = res }()
			t, err := s.store.GetTarget(parent, rem.TargetID)
			if err != nil {
				res.State, res.Detail = "failed", err.Error()
				return
			}
			if t.Visibility != "" {
				res.State, res.Detail = "kept", "target "+t.Name+" sets visibility "+t.Visibility+" for all its mirrors"
				return
			}
			p, err := provider.Get(t.Provider)
			if err != nil {
				res.State, res.Detail = "failed", err.Error()
				return
			}
			vs, ok := p.(provider.VisibilitySetter)
			if !ok || rem.URL != "" {
				res.State, res.Detail = "unsupported", "no API to change it: change the visibility on the platform"
				return
			}
			ctx, cancel := context.WithTimeout(parent, 30*time.Second)
			defer cancel()
			secret, err := s.targetSecret(ctx, t)
			if err != nil {
				res.State, res.Detail = "failed", err.Error()
				return
			}
			spec := provider.RepoSpec{Namespace: rem.Namespace, Name: rem.Name, Visibility: vis}
			if err := vs.SetVisibility(ctx, t, secret, spec, vis); err != nil {
				res.State, res.Detail = "failed", err.Error()
				s.log.Error("change mirror visibility", "repo", repo.Slug, "mirror", rem.Label(), "err", err)
				return
			}
			res.State = "changed"
		}(i, rem)
	}
	wg.Wait()
	return results
}
