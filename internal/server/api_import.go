package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/qwq-inc/gitrepo/internal/registry"
	"github.com/qwq-inc/gitrepo/internal/store"
	"github.com/qwq-inc/gitrepo/internal/syncer"
)

// apiImport makes a hub repository a copy of one of its mirrors (creating
// the repository and the mirror entry when needed) and then syncs the
// other mirrors. It serves onboarding of existing projects and rebuilding
// a lost hub.
func (s *Server) apiImport(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Target     string `json:"target"`
		Namespace  string `json:"namespace"`
		Name       string `json:"name"`
		URL        string `json:"url"`
		Force      bool   `json:"force"`
		Visibility string `json:"visibility"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	if in.Target == "" {
		s.fail(w, badRequest("target is required (the platform account that holds the existing repository)"))
		return
	}
	t, err := s.store.GetTargetByName(r.Context(), in.Target)
	if err != nil {
		s.fail(w, badRequest("target %q not found", in.Target))
		return
	}
	slug := r.PathValue("org") + "/" + strings.TrimSuffix(r.PathValue("name"), ".git")
	repo, _, err := s.reg.CreateRepo(r.Context(), slug, in.Visibility, true)
	if err != nil {
		s.fail(w, badRequest("%v", err))
		return
	}
	_, repoName, _ := strings.Cut(repo.Slug, "/")
	name := in.Name
	if name == "" {
		name = repoName
	}

	// Reuse the mirror entry for this location if there is one.
	remotes, err := s.store.ListRemotes(r.Context(), repo.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	var rem *store.Remote
	for _, x := range remotes {
		if x.TargetID != t.ID {
			continue
		}
		if in.URL != "" && x.URL == in.URL ||
			in.URL == "" && x.URL == "" && strings.EqualFold(x.Name, name) && (in.Namespace == "" || strings.EqualFold(x.EffectiveNamespace(), in.Namespace)) {
			rem = x
			break
		}
	}
	if rem == nil {
		rem, err = s.reg.AddRemote(r.Context(), repo, t.Name, in.Namespace, name, in.URL, "", true)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, registry.ErrLocationTaken) {
				status = http.StatusConflict
			}
			s.fail(w, &apiError{status: status, err: err})
			return
		}
	}

	res, err := s.mgr.Import(r.Context(), repo, rem, in.Force)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, syncer.ErrHubNotEmpty) || errors.Is(err, syncer.ErrConcurrentChange) {
			status = http.StatusConflict
		}
		s.fail(w, &apiError{status: status, err: err})
		return
	}
	s.log.Info("repository imported", "repo", repo.Slug, "from", rem.Label(), "refs", res.Refs, "lfs_objects", res.LFSObjects)
	if s.bundles != nil && res.BackedUp > 0 {
		s.bundles.Remove(repo.Slug)
	}
	s.mgr.Enqueue(repo.ID, syncer.Request{Trigger: syncer.TriggerManual})
	ro, err := s.repoOut(r.Context(), repo, false)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"import": res, "source": rem.Label(), "repo": ro})
}
