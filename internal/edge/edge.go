// Package edge runs a read-only replica of a Gitrepo hub close to its
// users. An edge node keeps a copy of every repository's branches and tags,
// the repository list and visibility, and the hashes and scopes of tokens
// and SSH keys, so it authorizes and serves clones, fetches, archives and
// Git LFS downloads by itself. Pushes are sent to the hub.
//
// It follows the hub by long-polling GET /api/edge/state. Every state is
// applied at once (credentials, repository list, visibility); fetching the
// repositories that changed happens in a separate worker pool, so a slow
// fetch never delays a revocation.
package edge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qwq-inc/gitrepo/internal/gitexec"
	"github.com/qwq-inc/gitrepo/internal/lfs"
	"github.com/qwq-inc/gitrepo/internal/registry"
	"github.com/qwq-inc/gitrepo/internal/store"
)

// State mirrors the hub's /api/edge/state answer.
type State struct {
	Version string `json:"version"`
	Repos   []struct {
		Slug          string `json:"slug"`
		Visibility    string `json:"visibility"`
		DefaultBranch string `json:"default_branch"`
		Version       string `json:"version"`
		Identity      string `json:"identity"`
		Purged        int64  `json:"purged"`
	} `json:"repos"`
	Tokens []struct {
		Name   string   `json:"name"`
		Hash   string   `json:"hash"`
		Scopes []string `json:"scopes"`
		Repos  []string `json:"repos"`
	} `json:"tokens"`
	SSHKeys []struct {
		Name        string   `json:"name"`
		Fingerprint string   `json:"fingerprint"`
		PublicKey   string   `json:"public_key"`
		Scopes      []string `json:"scopes"`
		Repos       []string `json:"repos"`
	} `json:"ssh_keys"`
}

// Replica follows a hub.
type Replica struct {
	// Upstream is the hub's base URL; Token an edge-scoped hub token.
	Upstream string
	Token    string
	Store    *store.Store
	Registry *registry.Registry
	Git      *gitexec.Git
	Log      *slog.Logger
	// Workers bounds concurrent repository fetches (default 4).
	Workers int
	// RefreshEvery re-fetches every repository (default 6h), healing any
	// change the hub failed to announce.
	RefreshEvery time.Duration

	mu       sync.Mutex
	repos    map[string]*repoState
	queue    chan string
	loaded   bool // credentials and repository list applied (persisted)
	revoked  bool // the hub refused our token (persisted)
	lastPoll time.Time
	lastErr  string
	fills    map[string]*fill
	fillQ    chan fillJob
	hubSem   chan struct{}
	missing  map[string]map[string]time.Time // slug -> oid -> expiry
	missingN int

	// Tests: replace the fetch, and observe the moment between a fetch
	// and the decision to go idle.
	fetchFunc      func(ctx context.Context, path, head string) error
	testAfterFetch func()
}

// repoState tracks one repository's copy.
type repoState struct {
	want struct {
		version, key, head, visibility string
	}
	// ready: a complete copy of the wanted identity is on disk.
	ready bool
	// busy: a worker owns the repository; dirty: run it again afterwards.
	busy, dirty bool
	// wipe: the copy must be discarded (deleted and re-created, or purged
	// on the hub) before anything is fetched or served again.
	wipe    bool
	deleted bool
	done    chan struct{}
}

type fill struct {
	done chan struct{}
	err  error
}

// sameHostClient refuses redirects to another host, so a link from the hub
// can never send the edge's token (or a request) elsewhere.
var sameHostClient = &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
	if len(via) > 0 && (req.URL.Host != via[0].URL.Host || req.URL.Scheme != via[0].URL.Scheme) {
		return errors.New("redirect to another host refused")
	}
	if len(via) >= 5 {
		return errors.New("too many redirects")
	}
	return nil
}}

const keySep = "|"

func repoKey(identity string, purged int64) string { return fmt.Sprint(identity, keySep, purged) }

func identityOf(key string) string { id, _, _ := strings.Cut(key, keySep); return id }

// Load restores the replica's state from the edge's database, so a
// restarted edge serves its copies at once, even if the hub is down.
func (r *Replica) Load(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.repos = map[string]*repoState{}
	r.fills = map[string]*fill{}
	r.fillQ = make(chan fillJob, fillQueue)
	r.hubSem = make(chan struct{}, hubQueries)
	r.missing = map[string]map[string]time.Time{}
	r.queue = make(chan string, 1<<16)
	if v, _ := r.Store.EdgeMeta(ctx, "loaded"); v == "1" {
		r.loaded = true
	}
	if v, _ := r.Store.EdgeMeta(ctx, "revoked"); v == "1" {
		r.revoked = true
	}
	repos, err := r.Store.ListRepos(ctx)
	if err != nil {
		return err
	}
	for _, rp := range repos {
		st := &repoState{ready: rp.UpstreamVersion != ""}
		st.want.version, st.want.key, st.want.head, st.want.visibility = rp.UpstreamVersion, rp.UpstreamKey, rp.DefaultBranch, rp.Visibility
		r.repos[rp.Slug] = st
	}
	return nil
}

// Run follows the hub until ctx ends. Load must have been called.
func (r *Replica) Run(ctx context.Context) {
	for i := 0; i < fillWorkers; i++ {
		go r.fillWorker(ctx)
	}
	workers := r.Workers
	if workers <= 0 {
		workers = 4
	}
	for i := 0; i < workers; i++ {
		go r.worker(ctx)
	}
	refresh := r.RefreshEvery
	if refresh <= 0 {
		refresh = 6 * time.Hour
	}
	nextRefresh := time.Now().Add(refresh)
	since := ""
	delay := time.Second
	for ctx.Err() == nil {
		st, status, err := r.poll(ctx, since, 50)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			r.mu.Lock()
			r.lastErr = err.Error()
			if status == http.StatusUnauthorized || status == http.StatusForbidden {
				// The hub refused us: the edge was decommissioned or its
				// token revoked. Serve nothing until it accepts us again.
				if !r.revoked {
					r.Log.Error("edge: the hub refused this edge's token; serving stopped", "status", status)
				}
				r.revoked = true
				r.Store.SetEdgeMeta(context.Background(), "revoked", "1")
			}
			r.mu.Unlock()
			r.Log.Warn("edge: cannot reach the hub", "upstream", r.Upstream, "err", err)
			sleep(ctx, delay)
			if delay *= 2; delay > 30*time.Second {
				delay = 30 * time.Second
			}
			continue
		}
		delay = time.Second
		force := time.Now().After(nextRefresh)
		if force {
			nextRefresh = time.Now().Add(refresh)
		}
		if err := r.apply(ctx, st, force); err != nil {
			r.Log.Error("edge: apply hub state", "err", err)
			since = ""
			sleep(ctx, 5*time.Second)
			continue
		}
		since = st.Version
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (r *Replica) poll(ctx context.Context, since string, wait int) (*State, int, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(wait+20)*time.Second)
	defer cancel()
	u := strings.TrimRight(r.Upstream, "/") + "/api/edge/state?wait=" + fmt.Sprint(wait) + "&since=" + url.QueryEscape(since)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+r.Token)
	req.Header.Set("User-Agent", "gitrepo-edge")
	resp, err := sameHostClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, resp.StatusCode, fmt.Errorf("hub answered %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var st State
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<20)).Decode(&st); err != nil {
		return nil, 0, fmt.Errorf("hub state: %w", err)
	}
	return &st, 0, nil
}

var visibilityRank = map[string]int{"public": 0, "internal": 1, "private": 2}

// apply makes credentials, the repository list and visibility match st at
// once, and queues fetches for repositories whose copy is behind.
func (r *Replica) apply(ctx context.Context, st *State, refreshAll bool) error {
	// Credentials first: a revoked token must stop working right away.
	var tokens []store.Token
	for _, t := range st.Tokens {
		tokens = append(tokens, store.Token{Name: t.Name, Hash: t.Hash, Scopes: t.Scopes, Repos: t.Repos})
	}
	if err := r.Store.ReplaceTokens(ctx, tokens); err != nil {
		return fmt.Errorf("replace tokens: %w", err)
	}
	var keys []store.SSHKey
	for _, k := range st.SSHKeys {
		keys = append(keys, store.SSHKey{Name: k.Name, Fingerprint: k.Fingerprint, PublicKey: k.PublicKey, Scopes: k.Scopes, Repos: k.Repos})
	}
	if err := r.Store.ReplaceSSHKeys(ctx, keys); err != nil {
		return fmt.Errorf("replace ssh keys: %w", err)
	}

	local, err := r.Store.ListRepos(ctx)
	if err != nil {
		return err
	}
	have := map[string]*store.Repo{}
	for _, rp := range local {
		have[rp.Slug] = rp
	}
	wanted := map[string]bool{}
	var schedule []string
	r.mu.Lock()
	defer func() {
		r.mu.Unlock()
		for _, slug := range schedule {
			r.schedule(slug)
		}
	}()
	for _, hr := range st.Repos {
		org, name, err := registry.ParseSlug(hr.Slug)
		if err != nil {
			continue
		}
		slug := org + "/" + name
		wanted[slug] = true
		key := repoKey(hr.Identity, hr.Purged)
		rs := r.repos[slug]
		if rs == nil {
			rs = &repoState{}
			r.repos[slug] = rs
		}
		cur := have[slug]
		switch {
		case cur == nil:
			cur = &store.Repo{Slug: slug, Visibility: hr.Visibility, DefaultBranch: hr.DefaultBranch, UpstreamKey: key}
			if err := r.Store.CreateRepo(ctx, cur); err != nil {
				return fmt.Errorf("add %s: %w", slug, err)
			}
			rs.ready, rs.wipe = false, true // nothing usable on disk yet
		case cur.UpstreamKey != "" && cur.UpstreamKey != key:
			// Deleted and re-created, or purged: the old copy must not be
			// served again (not even an object by SHA).
			if identityOf(cur.UpstreamKey) != hr.Identity {
				r.Log.Info("edge: repository re-created on the hub; discarding the old copy", "repo", slug)
			} else {
				r.Log.Info("edge: backups purged on the hub; rebuilding the copy", "repo", slug)
			}
			rs.ready, rs.wipe = false, true
			cur.Visibility, cur.UpstreamKey, cur.UpstreamVersion = hr.Visibility, key, ""
			if err := r.Store.UpdateRepo(ctx, cur); err != nil {
				return err
			}
			if err := r.Store.SetRepoUpstream(ctx, cur.ID, key, ""); err != nil {
				return err
			}
		default:
			if cur.UpstreamKey == "" {
				r.Store.SetRepoUpstream(ctx, cur.ID, key, cur.UpstreamVersion)
			}
			// More restrictive visibility applies now; less restrictive
			// only once the copy is the current one.
			if cur.Visibility != hr.Visibility && (visibilityRank[hr.Visibility] > visibilityRank[cur.Visibility] || cur.UpstreamVersion == hr.Version) {
				cur.Visibility = hr.Visibility
				if err := r.Store.UpdateRepo(ctx, cur); err != nil {
					return err
				}
			}
		}
		if rs.want.version != hr.Version {
			r.forgetMissing(slug)
		}
		rs.deleted = false
		rs.want.version, rs.want.key, rs.want.head, rs.want.visibility = hr.Version, key, hr.DefaultBranch, hr.Visibility
		if refreshAll || cur.UpstreamVersion != hr.Version || rs.wipe {
			schedule = append(schedule, slug)
		}
	}
	for slug, rp := range have {
		if wanted[slug] {
			continue
		}
		if err := r.Store.DeleteRepo(ctx, rp.ID); err != nil {
			return fmt.Errorf("remove %s: %w", slug, err)
		}
		rs := r.repos[slug]
		if rs != nil && rs.busy {
			rs.deleted, rs.ready, rs.wipe = true, false, true // the worker removes it
		} else {
			os.RemoveAll(r.Registry.Path(slug))
			delete(r.repos, slug)
		}
		r.Log.Info("edge: repository removed on the hub", "repo", slug)
	}
	r.loaded, r.revoked = true, false
	r.lastPoll, r.lastErr = time.Now(), ""
	r.Store.SetEdgeMeta(ctx, "loaded", "1")
	r.Store.SetEdgeMeta(ctx, "revoked", "0")
	return nil
}

// schedule queues a fetch of slug (or marks the running one to repeat).
func (r *Replica) schedule(slug string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rs := r.repos[slug]
	if rs == nil {
		return
	}
	if rs.busy {
		rs.dirty = true
		return
	}
	select {
	case r.queue <- slug:
		rs.busy, rs.done = true, make(chan struct{})
	default: // full: the periodic refresh catches up
	}
}

func (r *Replica) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case slug := <-r.queue:
			r.run(ctx, slug)
		}
	}
}

// run brings one repository up to date, repeating while changes arrive.
func (r *Replica) run(ctx context.Context, slug string) {
	path := r.Registry.Path(slug)
	for {
		r.mu.Lock()
		rs := r.repos[slug]
		if rs == nil {
			r.mu.Unlock()
			return
		}
		want, wipe, deleted := rs.want, rs.wipe, rs.deleted
		rs.wipe, rs.dirty = false, false
		r.mu.Unlock()

		if wipe {
			os.RemoveAll(path)
		}
		var err error
		if !deleted {
			if r.fetchFunc != nil {
				err = r.fetchFunc(ctx, path, want.head)
			} else {
				err = r.fetch(ctx, path, want.head)
			}
		}

		r.mu.Lock()
		if deleted || rs.deleted {
			os.RemoveAll(path)
			if r.repos[slug] == rs {
				delete(r.repos, slug)
			}
			close(rs.done)
			r.mu.Unlock()
			return
		}
		current := err == nil && !rs.wipe && rs.want.key == want.key
		if current {
			rs.ready = true
		}
		r.mu.Unlock()

		if current {
			// Record the copy, then allow a less restrictive visibility.
			if rp, gerr := r.Store.GetRepoBySlug(ctx, slug); gerr == nil {
				r.Store.SetRepoUpstream(ctx, rp.ID, want.key, want.version)
				if rp.Visibility != want.visibility || rp.DefaultBranch != want.head {
					rp.Visibility, rp.DefaultBranch = want.visibility, want.head
					r.Store.UpdateRepo(ctx, rp)
				}
			}
		}
		if r.testAfterFetch != nil {
			r.testAfterFetch()
		}
		// Decide whether to stop in the same critical section that marks the
		// repository idle: a change arriving after the fetch (schedule only
		// sets dirty while we are busy) must not be lost.
		r.mu.Lock()
		if (rs.dirty || rs.wipe) && ctx.Err() == nil {
			r.mu.Unlock()
			continue
		}
		rs.busy = false
		close(rs.done)
		r.mu.Unlock()
		if err != nil && ctx.Err() == nil {
			r.Log.Warn("edge: fetch failed; retrying", "repo", slug, "err", err)
			time.AfterFunc(10*time.Second, func() {
				if ctx.Err() == nil {
					r.schedule(slug)
				}
			})
		}
		return
	}
}

func (r *Replica) cred() *gitexec.Cred {
	return &gitexec.Cred{Kind: "token", Username: "edge", Secret: r.Token}
}

func (r *Replica) fetch(ctx context.Context, path, head string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err != nil {
		if err := r.Git.InitBare(ctx, path, "main"); err != nil {
			return err
		}
	}
	slug := strings.TrimSuffix(strings.TrimPrefix(path, r.Registry.ReposDir+string(filepath.Separator)), ".git")
	if err := r.Git.MirrorFetch(ctx, path, strings.TrimRight(r.Upstream, "/")+"/"+filepath.ToSlash(slug)+".git", r.cred()); err != nil {
		return err
	}
	if head != "" {
		return r.Git.SetHead(ctx, path, "refs/heads/"+head)
	}
	return nil
}

// Ready reports whether the edge can serve: it has a copy of the hub's
// state and the hub has not refused it.
func (r *Replica) Ready() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loaded && !r.revoked
}

// RepoReady reports whether slug can be served: a complete copy of the
// repository the hub currently has under that name is on disk.
func (r *Replica) RepoReady(slug string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	rs := r.repos[slug]
	return r.loaded && !r.revoked && rs != nil && rs.ready && !rs.wipe
}

// WaitFresh waits (at most max) for a running fetch of slug, so a client
// that pushed to the hub and then fetches from the edge sees its push.
func (r *Replica) WaitFresh(ctx context.Context, slug string, max time.Duration) {
	r.mu.Lock()
	var ch chan struct{}
	if rs := r.repos[slug]; rs != nil && rs.busy {
		ch = rs.done
	}
	r.mu.Unlock()
	if ch == nil {
		return
	}
	t := time.NewTimer(max)
	defer t.Stop()
	select {
	case <-ch:
	case <-t.C:
	case <-ctx.Done():
	}
}

// Status describes the replica (metrics, health).
type Status struct {
	Upstream  string
	Ready     bool
	Revoked   bool
	LastPoll  time.Time
	LastError string
	Pending   int
}

func (r *Replica) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	pending := 0
	for _, rs := range r.repos {
		if rs.busy || !rs.ready {
			pending++
		}
	}
	return Status{Upstream: r.Upstream, Ready: r.loaded && !r.revoked, Revoked: r.revoked, LastPoll: r.lastPoll, LastError: r.lastErr, Pending: pending}
}

// ------------------------------------------------------------------- LFS

const (
	// hubQueries bounds concurrent LFS batch requests to the hub; fillQueue
	// and fillWorkers bound the background downloads they start.
	hubQueries  = 8
	fillQueue   = 1024
	fillWorkers = 8
	// missingTTL: how long the hub's "no such object" answer is trusted.
	// A push to the repository clears it sooner.
	missingTTL = 30 * time.Second
	maxMissing = 1 << 14
)

// errFillQueueFull: the background queue was full; a download fetches the
// object itself.
var errFillQueueFull = errors.New("fill queue full")

type fillJob struct {
	repo lfs.Repo
	oid  string
	link hubLink
	f    *fill
}

// hubLink is a download link from the hub's batch answer.
type hubLink struct {
	size    int64
	href    string
	header  map[string]string
	expires time.Time
}

func fillKey(repo lfs.Repo, oid string) string { return repo.Dir + "|" + oid }

func stored(repo lfs.Repo, oid string) bool {
	fi, err := os.Stat(lfs.ObjectPath(repo.Dir, oid))
	return err == nil && fi.Mode().IsRegular()
}

// FillLFS asks the hub, in one batch request, which of the objects a
// client's download batch needs it has, queues background downloads of
// those (deduplicated, on a bounded queue) and reports them with their
// sizes. Objects the hub does not have are remembered for a short while,
// so a client listing made-up IDs costs one hub request, not one per ID.
func (r *Replica) FillLFS(ctx context.Context, repo lfs.Repo, objects []lfs.Object) (map[string]int64, error) {
	have := map[string]int64{}
	var ask []lfs.Object
	for _, o := range objects {
		if !r.knownMissing(repo, o.OID) {
			ask = append(ask, o)
		}
	}
	if len(ask) == 0 {
		return have, nil
	}
	links, err := r.queryHub(ctx, repo, ask)
	if err != nil {
		return have, err
	}
	for oid, l := range links {
		have[oid] = l.size
		r.enqueueFill(repo, oid, l)
	}
	return have, nil
}

// enqueueFill queues a background download of oid unless one is running.
func (r *Replica) enqueueFill(repo lfs.Repo, oid string, l hubLink) {
	key := fillKey(repo, oid)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fills[key] != nil {
		return
	}
	f := &fill{done: make(chan struct{})}
	select {
	case r.fillQ <- fillJob{repo: repo, oid: oid, link: l, f: f}:
		r.fills[key] = f
	default:
		// Full: the client's download fetches it (AwaitLFS).
	}
}

// fillWorker runs queued background downloads.
func (r *Replica) fillWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-r.fillQ:
			var err error
			if !stored(j.repo, j.oid) {
				jctx, cancel := context.WithTimeout(ctx, time.Hour)
				if time.Now().Before(j.link.expires) {
					err = r.download(jctx, j.repo, j.oid, j.link.href, j.link.header)
				} else {
					err = r.fetchLFS(jctx, j.repo, j.oid)
				}
				cancel()
			}
			r.finishFill(fillKey(j.repo, j.oid), j.f, err)
		}
	}
}

func (r *Replica) finishFill(key string, f *fill, err error) {
	r.mu.Lock()
	if r.fills[key] == f {
		delete(r.fills, key)
	}
	f.err = err
	close(f.done)
	r.mu.Unlock()
}

// AwaitLFS waits until oid is stored: it waits for a running download, or
// fetches the object itself (in the client's request, so the number of
// these is bounded by the clients waiting).
func (r *Replica) AwaitLFS(ctx context.Context, repo lfs.Repo, oid string) error {
	key := fillKey(repo, oid)
	for attempt := 0; attempt < 2; attempt++ {
		if stored(repo, oid) {
			return nil
		}
		if r.knownMissing(repo, oid) {
			return lfs.ErrNotOnHub
		}
		r.mu.Lock()
		f := r.fills[key]
		if f == nil {
			f = &fill{done: make(chan struct{})}
			r.fills[key] = f
			r.mu.Unlock()
			err := r.fetchLFS(ctx, repo, oid)
			r.finishFill(key, f, err)
			if err != nil {
				return err
			}
			continue
		}
		r.mu.Unlock()
		select {
		case <-f.done:
			// Someone else's download failed (perhaps their client went
			// away): try once more ourselves.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if stored(repo, oid) {
		return nil
	}
	return lfs.ErrNotOnHub
}

// fetchLFS fetches one object whose size is not known.
func (r *Replica) fetchLFS(ctx context.Context, repo lfs.Repo, oid string) error {
	if stored(repo, oid) {
		return nil
	}
	links, err := r.queryHub(ctx, repo, []lfs.Object{{OID: oid, Size: 0}}) // 0: size unknown
	if err != nil {
		return err
	}
	l, ok := links[oid]
	if !ok {
		return lfs.ErrNotOnHub
	}
	return r.download(ctx, repo, oid, l.href, l.header)
}

// queryHub sends one download batch to the hub and returns the links it
// gives; objects it reports missing are remembered (knownMissing).
func (r *Replica) queryHub(ctx context.Context, repo lfs.Repo, objects []lfs.Object) (map[string]hubLink, error) {
	select {
	case r.hubSem <- struct{}{}:
		defer func() { <-r.hubSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	body, _ := json.Marshal(map[string]any{"operation": "download", "transfers": []string{"basic"}, "objects": objects})
	base := strings.TrimRight(r.Upstream, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/"+repo.Slug+".git/info/lfs/objects/batch", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth("edge", r.Token)
	req.Header.Set("Accept", "application/vnd.git-lfs+json")
	req.Header.Set("Content-Type", "application/vnd.git-lfs+json")
	resp, err := sameHostClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hub LFS batch: %s", resp.Status)
	}
	var out struct {
		Objects []struct {
			OID     string `json:"oid"`
			Size    int64  `json:"size"`
			Actions struct {
				Download *struct {
					Href      string            `json:"href"`
					Header    map[string]string `json:"header"`
					ExpiresIn int               `json:"expires_in"`
				} `json:"download"`
			} `json:"actions"`
			Error *struct {
				Code int `json:"code"`
			} `json:"error"`
		} `json:"objects"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&out); err != nil {
		return nil, err
	}
	asked := map[string]bool{}
	for _, o := range objects {
		asked[o.OID] = true
	}
	up, _ := url.Parse(base)
	links := map[string]hubLink{}
	var missing []string
	for _, o := range out.Objects {
		if !asked[o.OID] {
			continue
		}
		if d := o.Actions.Download; d != nil {
			h, err := url.Parse(d.Href)
			if err != nil || up == nil || h.Scheme != up.Scheme || h.Host != up.Host {
				r.Log.Warn("edge: the hub returned an LFS link on another host; ignored", "repo", repo.Slug)
				continue
			}
			ttl := time.Duration(d.ExpiresIn) * time.Second
			if ttl <= 0 {
				ttl = 5 * time.Minute
			}
			links[o.OID] = hubLink{size: o.Size, href: d.Href, header: d.Header, expires: time.Now().Add(ttl - ttl/4)}
		} else if o.Error != nil && o.Error.Code == http.StatusNotFound {
			missing = append(missing, o.OID)
		}
	}
	r.markMissing(repo, missing)
	return links, nil
}

func (r *Replica) knownMissing(repo lfs.Repo, oid string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	exp, ok := r.missing[repo.Slug][oid]
	return ok && time.Now().Before(exp)
}

func (r *Replica) markMissing(repo lfs.Repo, oids []string) {
	if len(oids) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.missingN+len(oids) > maxMissing {
		now := time.Now()
		r.missingN = 0
		for slug, m := range r.missing {
			for oid, exp := range m {
				if now.After(exp) {
					delete(m, oid)
				}
			}
			if len(m) == 0 {
				delete(r.missing, slug)
			}
			r.missingN += len(m)
		}
		if r.missingN+len(oids) > maxMissing {
			r.missing, r.missingN = map[string]map[string]time.Time{}, 0
		}
	}
	m := r.missing[repo.Slug]
	if m == nil {
		m = map[string]time.Time{}
		r.missing[repo.Slug] = m
	}
	exp := time.Now().Add(missingTTL)
	for _, oid := range oids {
		if _, ok := m[oid]; !ok {
			r.missingN++
		}
		m[oid] = exp
	}
}

// forgetMissing drops what is remembered as missing for slug (it changed:
// LFS objects are uploaded before the push that references them).
// r.mu must be held.
func (r *Replica) forgetMissing(slug string) {
	r.missingN -= len(r.missing[slug])
	delete(r.missing, slug)
}

func (r *Replica) download(ctx context.Context, repo lfs.Repo, oid, href string, header map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, href, nil)
	if err != nil {
		return err
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := sameHostClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: %s", resp.Status)
	}
	dst := lfs.ObjectPath(repo.Dir, oid)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".edge-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	hash := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, hash), resp.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != oid {
		return errors.New("content does not match the object ID")
	}
	return os.Rename(tmp.Name(), dst)
}

// ProxyLFS forwards an LFS request (upload batches, lock checks) to the hub
// with the client's own credentials; the answer's links point to the hub,
// so the client uploads there directly. It works for repositories the edge
// has not heard of yet (a push creating one).
func (r *Replica) ProxyLFS(w http.ResponseWriter, req *http.Request, slug, rest string, body []byte) {
	up, err := http.NewRequestWithContext(req.Context(), req.Method,
		strings.TrimRight(r.Upstream, "/")+"/"+slug+".git/info/lfs/"+rest, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for _, h := range []string{"Authorization", "Accept", "Content-Type", "User-Agent"} {
		if v := req.Header.Get(h); v != "" {
			up.Header.Set(h, v)
		}
	}
	resp, err := sameHostClient.Do(up)
	if err != nil {
		w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `{"message":%q}`, "gitrepo edge: the hub is unreachable for LFS uploads")
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "LFS-Authenticate", "WWW-Authenticate"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, io.LimitReader(resp.Body, 16<<20))
}
