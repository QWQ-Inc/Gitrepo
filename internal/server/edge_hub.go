package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Hub side of edge replication. Edge nodes long-poll GET /api/edge/state:
// the answer carries a state version, the repositories (each with its own
// version), and the token and SSH key hashes they need to authorize reads.
// Pushes and every successful admin change advance the version and wake
// the waiting edges, which then fetch only the repositories that changed.

type stateClock struct {
	mu    sync.Mutex
	nonce string // per process: after a restart edges re-check everything
	gen   uint64
	repos map[string]uint64
	ch    chan struct{}
}

func newStateClock() *stateClock {
	b := make([]byte, 6)
	rand.Read(b)
	return &stateClock{nonce: hex.EncodeToString(b), repos: map[string]uint64{}, ch: make(chan struct{})}
}

// bump records a change (of repository slug, if not empty) and wakes the
// waiting edges.
func (c *stateClock) bump(slug string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	if slug != "" {
		c.repos[slug] = c.gen
	}
	close(c.ch)
	c.ch = make(chan struct{})
}

func (c *stateClock) version() (string, chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nonce + ":" + strconv.FormatUint(c.gen, 10), c.ch
}

func (c *stateClock) repoVersion(slug string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nonce + ":" + strconv.FormatUint(c.repos[slug], 10)
}

// EdgeState is what an edge node replicates.
// The bootstrap admin token is deliberately not replicated: it is chosen
// by a person (not random), so its unsalted hash could be brute-forced.
type EdgeState struct {
	Version string      `json:"version"`
	Repos   []EdgeRepo  `json:"repos"`
	Tokens  []EdgeToken `json:"tokens"`
	SSHKeys []EdgeKey   `json:"ssh_keys"`
}

type EdgeRepo struct {
	Slug          string `json:"slug"`
	Visibility    string `json:"visibility"`
	DefaultBranch string `json:"default_branch,omitempty"`
	// Version changes whenever the repository's branches or tags may have.
	Version string `json:"version"`
	// Identity changes when the slug is deleted and created again; Purged
	// when backups were purged. Either way an edge rebuilds its copy so
	// nothing removed on the hub stays fetchable there.
	Identity string `json:"identity"`
	Purged   int64  `json:"purged,omitempty"`
}

type EdgeToken struct {
	Name   string   `json:"name"`
	Hash   string   `json:"hash"`
	Scopes []string `json:"scopes"`
	Repos  []string `json:"repos,omitempty"`
}

type EdgeKey struct {
	Name        string   `json:"name"`
	Fingerprint string   `json:"fingerprint"`
	PublicKey   string   `json:"public_key"`
	Scopes      []string `json:"scopes"`
	Repos       []string `json:"repos,omitempty"`
}

func (s *Server) registerEdgeAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/edge/state", s.apiEdgeState)
}

func (s *Server) apiEdgeState(w http.ResponseWriter, r *http.Request) {
	p, err := s.auth.FromRequest(r)
	switch {
	case err != nil || p == nil:
		w.Header().Set("WWW-Authenticate", `Bearer realm="gitrepo"`)
		writeErr(w, http.StatusUnauthorized, errors.New("edge token required"))
		return
	case !p.IsEdge():
		writeErr(w, http.StatusForbidden, errors.New("edge scope required"))
		return
	case !p.IsAdmin() && len(p.Repos) > 0:
		writeErr(w, http.StatusForbidden, errors.New("an edge token must cover all repositories (create it without --repos)"))
		return
	}
	version, changed := s.clock.version()
	if since := r.URL.Query().Get("since"); since == version {
		wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
		if wait > 55 {
			wait = 55
		}
		if wait > 0 {
			t := time.NewTimer(time.Duration(wait) * time.Second)
			select {
			case <-changed:
			case <-t.C:
			case <-s.stopping:
			case <-r.Context().Done():
				t.Stop()
				return
			}
			t.Stop()
			version, _ = s.clock.version()
		}
	}
	ctx := r.Context()
	st := EdgeState{Version: version, Repos: []EdgeRepo{}, Tokens: []EdgeToken{}, SSHKeys: []EdgeKey{}}
	repos, err := s.store.ListRepos(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, rp := range repos {
		st.Repos = append(st.Repos, EdgeRepo{
			Slug: rp.Slug, Visibility: rp.Visibility, DefaultBranch: rp.DefaultBranch, Version: s.clock.repoVersion(rp.Slug),
			Identity: strconv.FormatInt(rp.ID, 10) + "@" + strconv.FormatInt(rp.CreatedAt, 10), Purged: rp.PurgedAt,
		})
	}
	tokens, err := s.store.ListTokens(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, t := range tokens {
		st.Tokens = append(st.Tokens, EdgeToken{Name: t.Name, Hash: t.Hash, Scopes: t.Scopes, Repos: t.Repos})
	}
	keys, err := s.store.ListSSHKeys(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, k := range keys {
		st.SSHKeys = append(st.SSHKeys, EdgeKey{Name: k.Name, Fingerprint: k.Fingerprint, PublicKey: k.PublicKey, Scopes: k.Scopes, Repos: k.Repos})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, st)
}
