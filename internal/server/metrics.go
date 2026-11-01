package server

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/qwq-inc/gitrepo/internal/store"
)

// handleMetrics renders Prometheus text exposition straight from the
// database; the values are cheap to compute and always consistent.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	repos, err := s.store.ListRepos(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	slugs := map[int64]string{}
	for _, repo := range repos {
		slugs[repo.ID] = repo.Slug
	}
	remotes, err := s.store.ListAllRemotes(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# HELP gitrepo_repos Number of registered repositories.\n# TYPE gitrepo_repos gauge\ngitrepo_repos %d\n", len(repos))
	if s.edge != nil {
		s.edgeMetrics(&b)
	}

	b.WriteString("# HELP gitrepo_remote_state Current state of each mirror (1 for the active state).\n# TYPE gitrepo_remote_state gauge\n")
	states := []string{store.StatePending, store.StateSynced, store.StateSyncing, store.StateBehind, store.StateDiverged, store.StateFailed, store.StatePaused}
	for _, rem := range remotes {
		for _, st := range states {
			v := 0
			if rem.State == st {
				v = 1
			}
			fmt.Fprintf(&b, "gitrepo_remote_state{repo=%q,remote=%q,state=%q} %d\n", slugs[rem.RepoID], rem.Label(), st, v)
		}
	}
	b.WriteString("# HELP gitrepo_remote_last_success_timestamp_seconds Last time the mirror matched the hub.\n# TYPE gitrepo_remote_last_success_timestamp_seconds gauge\n")
	for _, rem := range remotes {
		fmt.Fprintf(&b, "gitrepo_remote_last_success_timestamp_seconds{repo=%q,remote=%q} %d\n", slugs[rem.RepoID], rem.Label(), rem.LastSuccessAt/1000)
	}
	b.WriteString("# HELP gitrepo_remote_consecutive_failures Failed sync attempts since the last success.\n# TYPE gitrepo_remote_consecutive_failures gauge\n")
	for _, rem := range remotes {
		fmt.Fprintf(&b, "gitrepo_remote_consecutive_failures{repo=%q,remote=%q} %d\n", slugs[rem.RepoID], rem.Label(), rem.ConsecutiveFailures)
	}
	targets, err := s.store.ListTargets(ctx)
	if err == nil {
		b.WriteString("# HELP gitrepo_target_credential_expiry_timestamp_seconds Configured credential expiry (0 = unknown).\n# TYPE gitrepo_target_credential_expiry_timestamp_seconds gauge\n")
		for _, t := range targets {
			fmt.Fprintf(&b, "gitrepo_target_credential_expiry_timestamp_seconds{target=%q} %d\n", t.Name, t.ExpiresAt/1000)
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.Write([]byte(b.String()))
}
