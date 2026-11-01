// Package edge runs a read-only replica of a Gitrepo hub close to its
// users. An edge node keeps a copy of every repository's branches and tags,
// the repository list and visibility, and the hashes and scopes of tokens
// and SSH keys, so it authorizes and serves clones, fetches, archives and
// Git LFS downloads by itself. Pushes are sent to the hub.
//
// It follows the hub by long-polling GET /api/edge/state: every push and
// admin change on the hub wakes it, and only repositories whose version
// changed are fetched.
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

	"github.com/qwq-inc/gitrepo/internal/auth"
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
	AdminHash string `json:"admin_hash"`
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
	// OnKeysChanged is called after SSH keys were replaced (to drop
	// connections of removed keys).
	OnKeysChanged func()

	mu       sync.Mutex
	version  string
	fetched  map[string]string        // slug -> repository version held
	pending  map[string]chan struct{} // closed when a running fetch ends
	ready    bool
	lastPoll time.Time
	lastErr  string
}

var httpClient = &http.Client{}

// Run follows the hub until ctx ends.
func (r *Replica) Run(ctx context.Context) {
	delay := time.Second
	for ctx.Err() == nil {
		r.mu.Lock()
		since := r.version
		r.mu.Unlock()
		st, err := r.poll(ctx, since, 50)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			r.setErr(err)
			r.Log.Warn("edge: cannot reach the hub", "upstream", r.Upstream, "err", err)
			sleep(ctx, delay)
			if delay *= 2; delay > 30*time.Second {
				delay = 30 * time.Second
			}
			continue
		}
		delay = time.Second
		if ok := r.apply(ctx, st); !ok {
			// Some fetches failed: ask again soon instead of waiting for
			// the next change.
			r.mu.Lock()
			r.version = ""
			r.mu.Unlock()
			sleep(ctx, 5*time.Second)
		}
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

func (r *Replica) setErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastErr = err.Error()
}

func (r *Replica) poll(ctx context.Context, since string, wait int) (*State, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(wait+20)*time.Second)
	defer cancel()
	u := strings.TrimRight(r.Upstream, "/") + "/api/edge/state?wait=" + fmt.Sprint(wait) + "&since=" + url.QueryEscape(since)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.Token)
	req.Header.Set("User-Agent", "gitrepo-edge")
	resp, err := httpClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("hub answered %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var st State
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<20)).Decode(&st); err != nil {
		return nil, fmt.Errorf("hub state: %w", err)
	}
	return &st, nil
}

// apply makes the local copy match st; it reports whether every fetch
// succeeded.
func (r *Replica) apply(ctx context.Context, st *State) bool {
	// Credentials first: a revoked token must stop working right away.
	var tokens []store.Token
	for _, t := range st.Tokens {
		tokens = append(tokens, store.Token{Name: t.Name, Hash: t.Hash, Scopes: t.Scopes, Repos: t.Repos})
	}
	if st.AdminHash != "" {
		tokens = append(tokens, store.Token{Name: "admin", Hash: st.AdminHash, Scopes: []string{auth.ScopeAdmin}})
	}
	if err := r.Store.ReplaceTokens(ctx, tokens); err != nil {
		r.Log.Error("edge: replace tokens", "err", err)
		return false
	}
	var keys []store.SSHKey
	for _, k := range st.SSHKeys {
		keys = append(keys, store.SSHKey{Name: k.Name, Fingerprint: k.Fingerprint, PublicKey: k.PublicKey, Scopes: k.Scopes, Repos: k.Repos})
	}
	if err := r.Store.ReplaceSSHKeys(ctx, keys); err != nil {
		r.Log.Error("edge: replace ssh keys", "err", err)
		return false
	}
	if r.OnKeysChanged != nil {
		r.OnKeysChanged()
	}

	local, err := r.Store.ListRepos(ctx)
	if err != nil {
		r.Log.Error("edge: list repositories", "err", err)
		return false
	}
	have := map[string]*store.Repo{}
	for _, rp := range local {
		have[rp.Slug] = rp
	}
	type job struct{ slug, version, head string }
	var jobs []job
	wanted := map[string]bool{}
	for _, rp := range st.Repos {
		if _, _, err := registry.ParseSlug(rp.Slug); err != nil {
			continue
		}
		wanted[rp.Slug] = true
		cur := have[rp.Slug]
		if cur == nil {
			cur = &store.Repo{Slug: rp.Slug, Visibility: rp.Visibility, DefaultBranch: rp.DefaultBranch}
			if err := r.Store.CreateRepo(ctx, cur); err != nil {
				r.Log.Error("edge: add repository", "repo", rp.Slug, "err", err)
				continue
			}
		} else if cur.Visibility != rp.Visibility || cur.DefaultBranch != rp.DefaultBranch {
			cur.Visibility, cur.DefaultBranch = rp.Visibility, rp.DefaultBranch
			if err := r.Store.UpdateRepo(ctx, cur); err != nil {
				r.Log.Error("edge: update repository", "repo", rp.Slug, "err", err)
			}
		}
		r.mu.Lock()
		if r.fetched == nil {
			r.fetched = map[string]string{}
			r.pending = map[string]chan struct{}{}
		}
		if r.fetched[rp.Slug] != rp.Version && r.pending[rp.Slug] == nil {
			r.pending[rp.Slug] = make(chan struct{})
			jobs = append(jobs, job{rp.Slug, rp.Version, rp.DefaultBranch})
		}
		r.mu.Unlock()
	}
	for slug, rp := range have {
		if wanted[slug] {
			continue
		}
		if err := r.Store.DeleteRepo(ctx, rp.ID); err != nil {
			r.Log.Error("edge: remove repository", "repo", slug, "err", err)
			continue
		}
		os.RemoveAll(r.Registry.Path(slug))
		r.mu.Lock()
		delete(r.fetched, slug)
		r.mu.Unlock()
		r.Log.Info("edge: repository removed on the hub", "repo", slug)
	}

	workers := r.Workers
	if workers <= 0 {
		workers = 4
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := true
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(j job) {
			defer wg.Done()
			defer func() { <-sem }()
			err := r.fetch(ctx, j.slug, j.head)
			r.mu.Lock()
			if err == nil {
				r.fetched[j.slug] = j.version
			}
			close(r.pending[j.slug])
			delete(r.pending, j.slug)
			r.mu.Unlock()
			if err != nil {
				r.Log.Warn("edge: fetch failed", "repo", j.slug, "err", err)
				mu.Lock()
				ok = false
				mu.Unlock()
			}
		}(j)
	}
	wg.Wait()
	r.mu.Lock()
	r.version = st.Version
	r.ready = true
	r.lastPoll = time.Now()
	if ok {
		r.lastErr = ""
	}
	r.mu.Unlock()
	return ok
}

func (r *Replica) cred() *gitexec.Cred {
	return &gitexec.Cred{Kind: "token", Username: "edge", Secret: r.Token}
}

func (r *Replica) fetch(ctx context.Context, slug, head string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	path := r.Registry.Path(slug)
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err != nil {
		if err := r.Git.InitBare(ctx, path, "main"); err != nil {
			return err
		}
	}
	if err := r.Git.MirrorFetch(ctx, path, strings.TrimRight(r.Upstream, "/")+"/"+slug+".git", r.cred()); err != nil {
		return err
	}
	if head != "" {
		return r.Git.SetHead(ctx, path, "refs/heads/"+head)
	}
	return nil
}

// Ready reports whether the first state from the hub has been applied.
func (r *Replica) Ready() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ready
}

// WaitFresh waits (at most max) for a running fetch of slug, so a client
// that pushed to the hub and then fetches from the edge sees its push.
func (r *Replica) WaitFresh(ctx context.Context, slug string, max time.Duration) {
	r.mu.Lock()
	ch := r.pending[slug]
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
	LastPoll  time.Time
	LastError string
	Pending   int
}

func (r *Replica) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Status{Upstream: r.Upstream, Ready: r.ready, LastPoll: r.lastPoll, LastError: r.lastErr, Pending: len(r.pending)}
}

// ------------------------------------------------------------------- LFS

// FillLFS downloads LFS objects the edge does not have yet from the hub,
// verifying each against its object ID.
func (r *Replica) FillLFS(ctx context.Context, repo lfs.Repo, objects []lfs.Object) error {
	body, _ := json.Marshal(map[string]any{"operation": "download", "transfers": []string{"basic"}, "objects": objects})
	base := strings.TrimRight(r.Upstream, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/"+repo.Slug+".git/info/lfs/objects/batch", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth("edge", r.Token)
	req.Header.Set("Accept", "application/vnd.git-lfs+json")
	req.Header.Set("Content-Type", "application/vnd.git-lfs+json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub LFS batch: %s", resp.Status)
	}
	var out struct {
		Objects []struct {
			OID     string `json:"oid"`
			Size    int64  `json:"size"`
			Actions struct {
				Download *struct {
					Href   string            `json:"href"`
					Header map[string]string `json:"header"`
				} `json:"download"`
			} `json:"actions"`
		} `json:"objects"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&out); err != nil {
		return err
	}
	up, _ := url.Parse(base)
	var errs []error
	for _, o := range out.Objects {
		d := o.Actions.Download
		if d == nil || !lfs.ValidOID(o.OID) {
			continue // not on the hub either
		}
		h, err := url.Parse(d.Href)
		if err != nil || up == nil || h.Scheme != up.Scheme || h.Host != up.Host {
			errs = append(errs, fmt.Errorf("%s: hub returned a download link on another host", o.OID))
			continue
		}
		if err := r.download(ctx, repo, o.OID, o.Size, d.Href, d.Header); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", o.OID, err))
		}
	}
	return errors.Join(errs...)
}

func (r *Replica) download(ctx context.Context, repo lfs.Repo, oid string, size int64, href string, header map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, href, nil)
	if err != nil {
		return err
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
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
	n, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(resp.Body, size+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != size || hex.EncodeToString(hash.Sum(nil)) != oid {
		return errors.New("content does not match the object ID")
	}
	return os.Rename(tmp.Name(), dst)
}

// ProxyLFSUpload forwards an LFS upload batch request to the hub with the
// client's own credentials; the answer's links point to the hub, so the
// client uploads there directly.
func (r *Replica) ProxyLFSUpload(w http.ResponseWriter, req *http.Request, repo lfs.Repo, body []byte) {
	up, err := http.NewRequestWithContext(req.Context(), http.MethodPost,
		strings.TrimRight(r.Upstream, "/")+"/"+repo.Slug+".git/info/lfs/objects/batch", bytes.NewReader(body))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for _, h := range []string{"Authorization", "Accept", "Content-Type", "User-Agent"} {
		if v := req.Header.Get(h); v != "" {
			up.Header.Set(h, v)
		}
	}
	resp, err := httpClient.Do(up)
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
