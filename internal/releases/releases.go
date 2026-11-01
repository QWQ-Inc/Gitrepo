// Package releases keeps releases on the hub and publishes them to the
// mirrors of a repository: GitHub, GitLab, Gitea/Forgejo and Gitee
// releases with notes and attached files.
//
// A release is published to a mirror only after Gitrepo has synced its tag
// there (the mirror's tag must point to the same object as the hub's), so
// the platform never creates a tag of its own. A release that already exists
// on a mirror and was not created by Gitrepo is reported as a conflict and
// left alone unless the release is marked to adopt it.
package releases

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/qwq-inc/gitrepo/internal/gitexec"
	"github.com/qwq-inc/gitrepo/internal/provider"
	"github.com/qwq-inc/gitrepo/internal/registry"
	"github.com/qwq-inc/gitrepo/internal/secrets"
	"github.com/qwq-inc/gitrepo/internal/store"
)

// Manager stores release assets and publishes releases.
type Manager struct {
	Store    *store.Store
	Git      *gitexec.Git
	Box      *secrets.Box
	Registry *registry.Registry
	Log      *slog.Logger
	// MaxAssetSize rejects larger uploads (0 = unlimited).
	MaxAssetSize int64
	// Timeout bounds publishing one release to one mirror.
	Timeout time.Duration

	mu      sync.Mutex
	locks   map[int64]*sync.Mutex
	queued  map[int64]bool
	queue   chan int64
	stop    chan struct{}
	stopped sync.WaitGroup
}

// Result is the outcome of publishing (or deleting) a release on a mirror.
type Result struct {
	RemoteID int64  `json:"remote_id"`
	Mirror   string `json:"mirror"`
	State    string `json:"state"`
	Error    string `json:"error,omitempty"`
}

// ErrInvalidName is returned for unusable asset names.
var ErrInvalidName = errors.New("invalid asset name")

// ErrTooLarge is returned for assets above MaxAssetSize.
var ErrTooLarge = errors.New("asset too large")

// Start runs the background publisher until Stop.
func (m *Manager) Start() {
	m.mu.Lock()
	m.queue = make(chan int64, 1024)
	m.stop = make(chan struct{})
	m.mu.Unlock()
	m.stopped.Add(1)
	go func() {
		defer m.stopped.Done()
		for {
			select {
			case <-m.stop:
				return
			case id := <-m.queue:
				m.mu.Lock()
				delete(m.queued, id)
				m.mu.Unlock()
				ctx, cancel := context.WithCancel(context.Background())
				go func() {
					select {
					case <-m.stop:
						cancel()
					case <-ctx.Done():
					}
				}()
				if _, err := m.Publish(ctx, id, false); err != nil && !errors.Is(err, store.ErrNotFound) {
					m.Log.Warn("publish release", "release_id", id, "err", err)
				}
				cancel()
			}
		}
	}()
}

// Stop ends the background publisher, cancelling a running publication.
func (m *Manager) Stop() {
	m.mu.Lock()
	stop := m.stop
	m.mu.Unlock()
	if stop != nil {
		close(stop)
		m.stopped.Wait()
	}
}

// Enqueue schedules a release for background publishing.
func (m *Manager) Enqueue(releaseID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.queue == nil || m.queued[releaseID] {
		return
	}
	if m.queued == nil {
		m.queued = map[int64]bool{}
	}
	select {
	case m.queue <- releaseID:
		m.queued[releaseID] = true
	default: // full: the patrol retries later
	}
}

// EnqueueRepo schedules the repository's published releases, e.g. after a
// sync may have brought tags to its mirrors.
func (m *Manager) EnqueueRepo(ctx context.Context, repoID int64) {
	rels, err := m.Store.ListReleases(ctx, repoID)
	if err != nil {
		return
	}
	for _, r := range rels {
		if r.Published {
			m.Enqueue(r.ID)
		}
	}
}

// RetryDue schedules releases with a failed, pending or waiting mirror.
func (m *Manager) RetryDue(ctx context.Context) {
	ids, err := m.Store.ReleasesToRetry(ctx, store.Now())
	if err != nil {
		m.Log.Warn("list releases to retry", "err", err)
		return
	}
	for _, id := range ids {
		m.Enqueue(id)
	}
}

func (m *Manager) lock(releaseID int64) func() {
	m.mu.Lock()
	if m.locks == nil {
		m.locks = map[int64]*sync.Mutex{}
	}
	l := m.locks[releaseID]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[releaseID] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// ------------------------------------------------------------------ assets

// BlobPath is where an asset's content is stored inside the hub repository
// (content-addressed, like Git LFS objects).
func BlobPath(repoPath, sha string) string {
	return filepath.Join(repoPath, "releases", "objects", sha[0:2], sha[2:4], sha)
}

// ValidAssetName accepts plain file names.
func ValidAssetName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 255 || strings.ContainsAny(name, `/\`) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

// PutAsset stores content as the release's asset name (replacing an
// existing one) and schedules publishing if the release is published.
func (m *Manager) PutAsset(ctx context.Context, repo *store.Repo, rel *store.Release, name, contentType string, content io.Reader) (*store.ReleaseAsset, error) {
	if !ValidAssetName(name) {
		return nil, ErrInvalidName
	}
	repoPath := m.Registry.Path(repo.Slug)
	tmpDir := filepath.Join(repoPath, "releases", "tmp")
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(tmpDir, "upload-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	limit := int64(math.MaxInt64)
	if m.MaxAssetSize > 0 {
		limit = m.MaxAssetSize + 1
	}
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(content, limit))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	if m.MaxAssetSize > 0 && n > m.MaxAssetSize {
		return nil, ErrTooLarge
	}
	sum := hex.EncodeToString(h.Sum(nil))
	dst := BlobPath(repoPath, sum)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return nil, err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	unlock := m.lock(rel.ID)
	defer unlock()
	var previous string
	if old, err := m.Store.ListReleaseAssets(ctx, rel.ID); err == nil {
		for _, a := range old {
			if a.Name == name {
				previous = a.SHA256
			}
		}
	}
	a := &store.ReleaseAsset{ReleaseID: rel.ID, Name: name, Size: n, SHA256: sum, ContentType: contentType}
	if err := m.Store.PutReleaseAsset(ctx, a); err != nil {
		return nil, err
	}
	if previous != "" && previous != sum {
		m.dropBlob(ctx, repo, previous)
	}
	m.Store.TouchRelease(ctx, rel.ID)
	if rel.Published {
		m.Enqueue(rel.ID)
	}
	return a, nil
}

// RemoveAsset deletes an asset from the release (and, once published, from
// the mirrors).
func (m *Manager) RemoveAsset(ctx context.Context, repo *store.Repo, rel *store.Release, name string) error {
	unlock := m.lock(rel.ID)
	defer unlock()
	assets, err := m.Store.ListReleaseAssets(ctx, rel.ID)
	if err != nil {
		return err
	}
	var sha string
	for _, a := range assets {
		if a.Name == name {
			sha = a.SHA256
		}
	}
	if err := m.Store.DeleteReleaseAsset(ctx, rel.ID, name); err != nil {
		return err
	}
	if sha != "" {
		m.dropBlob(ctx, repo, sha)
	}
	m.Store.TouchRelease(ctx, rel.ID)
	if rel.Published {
		m.Enqueue(rel.ID)
	}
	return nil
}

// OpenAsset opens an asset's content.
func (m *Manager) OpenAsset(repo *store.Repo, a *store.ReleaseAsset) (*os.File, error) {
	return os.Open(BlobPath(m.Registry.Path(repo.Slug), a.SHA256))
}

func (m *Manager) dropBlob(ctx context.Context, repo *store.Repo, sha string) {
	if inUse, err := m.Store.ReleaseBlobInUse(ctx, repo.ID, sha); err == nil && !inUse {
		os.Remove(BlobPath(m.Registry.Path(repo.Slug), sha))
	}
}

// ------------------------------------------------------------- publishing

// contentHash identifies what was last published, so unchanged releases
// cost no API calls.
func contentHash(rel *store.Release, assets []*store.ReleaseAsset) string {
	type a struct{ Name, SHA string }
	list := make([]a, 0, len(assets))
	for _, x := range assets {
		list = append(list, a{x.Name, x.SHA256})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	raw, _ := json.Marshal(struct {
		Tag, Name, Body string
		Pre             bool
		Assets          []a
	}{rel.Tag, rel.Name, rel.Body, rel.Prerelease, list})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Publish brings every mirror of the release up to date and returns the
// per-mirror results. Unpublished releases are left alone. Mirrors already
// holding this exact content are skipped unless force (e.g. after the
// release was deleted on a platform by hand).
func (m *Manager) Publish(ctx context.Context, releaseID int64, force bool) ([]Result, error) {
	unlock := m.lock(releaseID)
	defer unlock()
	rel, err := m.Store.GetRelease(ctx, releaseID)
	if err != nil {
		return nil, err
	}
	if !rel.Published {
		return nil, nil
	}
	repo, err := m.Store.GetRepo(ctx, rel.RepoID)
	if err != nil {
		return nil, err
	}
	assets, err := m.Store.ListReleaseAssets(ctx, rel.ID)
	if err != nil {
		return nil, err
	}
	remotes, err := m.Store.ListRemotes(ctx, repo.ID)
	if err != nil {
		return nil, err
	}
	repoPath := m.Registry.Path(repo.Slug)
	tagRef := "refs/tags/" + rel.Tag
	hubRefs, err := m.Git.ForEachRef(ctx, repoPath, tagRef)
	if err != nil {
		return nil, err
	}
	tagObj := hubRefs[tagRef]
	commit := ""
	if tagObj != "" {
		if out, err := m.Git.Run(ctx, repoPath, "rev-parse", "--verify", "--quiet", tagObj+"^{commit}"); err == nil {
			commit = strings.TrimSpace(string(out))
		}
	}
	hash := contentHash(rel, assets)
	var results []Result
	for _, rem := range remotes {
		res := m.publishTo(ctx, repo, rel, assets, rem, tagObj, commit, hash, force)
		results = append(results, res)
	}
	return results, nil
}

func (m *Manager) publishTo(parent context.Context, repo *store.Repo, rel *store.Release, assets []*store.ReleaseAsset, rem *store.Remote, tagObj, commit, hash string, force bool) Result {
	res := Result{RemoteID: rem.ID, Mirror: rem.Label()}
	st, err := m.Store.GetReleaseMirror(parent, rel.ID, rem.ID)
	if errors.Is(err, store.ErrNotFound) {
		st = &store.ReleaseMirror{ReleaseID: rel.ID, RemoteID: rem.ID, State: store.RelPending}
	} else if err != nil {
		res.State, res.Error = store.RelFailed, err.Error()
		return res
	}
	save := func(state, msg string) Result {
		st.State, st.LastError = state, msg
		switch state {
		case store.RelFailed:
			st.Failures++
			st.NextRetryAt = store.FromTime(time.Now().Add(backoff(st.Failures)))
		case store.RelSynced:
			st.Failures, st.NextRetryAt, st.SyncedHash = 0, 0, hash
		default:
			st.Failures, st.NextRetryAt = 0, 0
		}
		if err := m.Store.SaveReleaseMirror(context.Background(), st); err != nil {
			m.Log.Warn("save release state", "release", rel.Tag, "mirror", rem.Label(), "err", err)
		}
		res.State, res.Error = state, msg
		return res
	}

	if !rem.Enabled {
		res.State = "paused"
		return res
	}
	prov, err := provider.Get(rem.Provider)
	if err != nil {
		return save(store.RelFailed, err.Error())
	}
	rp, ok := prov.(provider.Releaser)
	if !ok || rem.URL != "" {
		return save(store.RelUnsupported, "releases are not supported for this mirror (platform without a release API, or mirror configured by URL)")
	}
	if st.State == store.RelSynced && st.SyncedHash == hash && !force {
		res.State = store.RelSynced
		return res
	}
	if tagObj == "" {
		return save(store.RelFailed, "tag "+rel.Tag+" does not exist on the hub")
	}
	mirrorRefs, err := m.Store.GetRefState(parent, rem.ID)
	if err != nil {
		return save(store.RelFailed, err.Error())
	}
	if mirrorRefs["refs/tags/"+rel.Tag] != tagObj {
		return save(store.RelWaiting, "waiting for the tag to be synced to this mirror")
	}

	timeout := m.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	t, err := m.Store.GetTarget(ctx, rem.TargetID)
	if err != nil {
		return save(store.RelFailed, err.Error())
	}
	secret := ""
	if len(t.CredSecret) > 0 {
		pt, err := m.Box.Open(t.CredSecret, t.CredentialAAD())
		if err != nil {
			return save(store.RelFailed, fmt.Sprintf("target %s: %v", t.Name, err))
		}
		secret = string(pt)
	}
	spec := provider.RepoSpec{Namespace: rem.Namespace, Name: rem.Name, Visibility: repo.Visibility}
	want := provider.Release{Tag: rel.Tag, Name: rel.Name, Body: rel.Body, Prerelease: rel.Prerelease, Commit: commit}

	existing, err := rp.GetRelease(ctx, t, secret, spec, rel.Tag)
	if err != nil {
		return save(store.RelFailed, err.Error())
	}
	var rr *provider.RemoteRelease
	switch {
	case existing != nil && st.RemoteRelease != "" && existing.ID == st.RemoteRelease:
		rr = existing // ours
	case existing != nil && !rel.Adopt:
		return save(store.RelConflict, "a release for "+rel.Tag+" already exists on this mirror and was not created by Gitrepo; delete it there, or publish with adopt to take it over")
	case existing != nil:
		rr = existing // adopted: its other files are left alone
		st.RemoteRelease = rr.ID
	default:
		// None there (never created, or deleted on the platform).
		if rr, err = rp.CreateRelease(ctx, t, secret, spec, want); err != nil {
			return save(store.RelFailed, err.Error())
		}
		st.RemoteRelease = rr.ID
		st.Assets = map[string]store.MirroredAsset{}
		st.SyncedHash = ""
	}
	if existing != nil {
		if err := rp.UpdateRelease(ctx, t, secret, spec, rr, want); err != nil {
			return save(store.RelFailed, err.Error())
		}
	}
	if st.Assets == nil {
		st.Assets = map[string]store.MirroredAsset{}
	}

	onMirror := map[string]provider.RemoteAsset{}
	for _, a := range rr.Assets {
		onMirror[a.Name] = a
	}
	wanted := map[string]bool{}
	repoPath := m.Registry.Path(repo.Slug)
	for _, a := range assets {
		wanted[a.Name] = true
		rec, ours := st.Assets[a.Name]
		cur, present := onMirror[a.Name]
		if ours && present && rec.ID == cur.ID && rec.SHA256 == a.SHA256 {
			continue
		}
		if present {
			// A stale copy, or a file of the same name someone else put
			// there: the platform refuses duplicate names.
			if err := rp.DeleteAsset(ctx, t, secret, spec, rr, cur.ID); err != nil {
				return save(store.RelFailed, fmt.Sprintf("replace %s: %v", a.Name, err))
			}
		}
		delete(st.Assets, a.Name)
		blob := BlobPath(repoPath, a.SHA256)
		up, err := rp.UploadAsset(ctx, t, secret, spec, rr, provider.Asset{
			Name: a.Name, ContentType: a.ContentType, Size: a.Size,
			Open: func() (io.ReadCloser, error) { return os.Open(blob) },
		})
		if err != nil {
			return save(store.RelFailed, fmt.Sprintf("upload %s: %v", a.Name, err))
		}
		st.Assets[a.Name] = store.MirroredAsset{ID: up.ID, SHA256: a.SHA256}
	}
	for name, rec := range st.Assets {
		if wanted[name] {
			continue
		}
		if _, present := onMirror[name]; present {
			if err := rp.DeleteAsset(ctx, t, secret, spec, rr, rec.ID); err != nil {
				return save(store.RelFailed, fmt.Sprintf("remove %s: %v", name, err))
			}
		}
		delete(st.Assets, name)
	}
	return save(store.RelSynced, "")
}

func backoff(failures int) time.Duration {
	d := 30 * time.Second
	for i := 1; i < failures && d < time.Hour; i++ {
		d *= 2
	}
	if d > time.Hour {
		d = time.Hour
	}
	return d
}

// --------------------------------------------------------------- deletion

// Delete removes the release from the hub. Unless hubOnly, it first deletes
// the releases Gitrepo created on the mirrors; if any of those fails, the
// hub release is kept so the deletion can be retried.
func (m *Manager) Delete(ctx context.Context, repo *store.Repo, rel *store.Release, hubOnly bool) ([]Result, error) {
	unlock := m.lock(rel.ID)
	defer unlock()
	var results []Result
	failed := false
	if !hubOnly {
		mirrors, err := m.Store.ListReleaseMirrors(ctx, rel.ID)
		if err != nil {
			return nil, err
		}
		for _, st := range mirrors {
			if st.RemoteRelease == "" {
				continue
			}
			rem, err := m.Store.GetRemote(ctx, st.RemoteID)
			if err != nil {
				continue
			}
			res := Result{RemoteID: rem.ID, Mirror: rem.Label(), State: "deleted"}
			if err := m.deleteOn(ctx, repo, rem, st); err != nil {
				res.State, res.Error = store.RelFailed, err.Error()
				failed = true
			} else {
				st.RemoteRelease, st.Assets, st.State, st.SyncedHash = "", nil, store.RelPending, ""
				m.Store.SaveReleaseMirror(ctx, st)
			}
			results = append(results, res)
		}
	}
	if failed {
		return results, errors.New("the release could not be deleted on every mirror; it is kept on the hub (retry, or delete it on the hub only)")
	}
	assets, _ := m.Store.ListReleaseAssets(ctx, rel.ID)
	if err := m.Store.DeleteRelease(ctx, rel.ID); err != nil {
		return results, err
	}
	for _, a := range assets {
		m.dropBlob(ctx, repo, a.SHA256)
	}
	return results, nil
}

func (m *Manager) deleteOn(parent context.Context, repo *store.Repo, rem *store.Remote, st *store.ReleaseMirror) error {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	prov, err := provider.Get(rem.Provider)
	if err != nil {
		return err
	}
	rp, ok := prov.(provider.Releaser)
	if !ok {
		return nil
	}
	t, err := m.Store.GetTarget(ctx, rem.TargetID)
	if err != nil {
		return err
	}
	secret := ""
	if len(t.CredSecret) > 0 {
		pt, err := m.Box.Open(t.CredSecret, t.CredentialAAD())
		if err != nil {
			return err
		}
		secret = string(pt)
	}
	spec := provider.RepoSpec{Namespace: rem.Namespace, Name: rem.Name, Visibility: repo.Visibility}
	return rp.DeleteRelease(ctx, t, secret, spec, &provider.RemoteRelease{ID: st.RemoteRelease})
}
