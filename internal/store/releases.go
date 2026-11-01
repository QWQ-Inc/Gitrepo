package store

import (
	"context"
	"encoding/json"
)

// Release is kept on the hub and published to the repository's mirrors.
type Release struct {
	ID         int64
	RepoID     int64
	Tag        string
	Name       string
	Body       string
	Prerelease bool
	// Published releases are sent to mirrors; unpublished ones (drafts)
	// exist only on the hub.
	Published bool
	// Adopt lets publishing take over a release that already exists on a
	// mirror for the same tag (otherwise that is reported as a conflict).
	Adopt     bool
	CreatedAt int64
	UpdatedAt int64
}

type ReleaseAsset struct {
	ID          int64
	ReleaseID   int64
	Name        string
	Size        int64
	SHA256      string
	ContentType string
	CreatedAt   int64
}

// Release mirror states.
const (
	RelPending     = "pending"
	RelWaiting     = "waiting" // the tag has not reached the mirror yet
	RelSynced      = "synced"
	RelFailed      = "failed"
	RelConflict    = "conflict" // the mirror has a release Gitrepo did not create
	RelUnsupported = "unsupported"
)

// MirroredAsset records an asset Gitrepo uploaded to a mirror. Name is the
// name the platform gave it (GitHub rewrites some characters).
type MirroredAsset struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
	Name   string `json:"name,omitempty"`
}

// ReleaseMirror is the publication state of a release on one remote.
type ReleaseMirror struct {
	ReleaseID int64
	RemoteID  int64
	// RemoteRelease identifies the platform release Gitrepo created or
	// adopted (see provider.RemoteRelease.Key).
	RemoteRelease string
	// Adopted releases were made by someone else: Gitrepo manages only its
	// own files there and never deletes the release itself.
	Adopted bool
	// Creating is set while a create request is in flight, so a release
	// whose creation response was lost is recognised as ours.
	Creating bool
	// AttemptHash is the content a failed or conflicting attempt tried to
	// publish; unchanged content is not retried before NextRetryAt.
	AttemptHash string
	State       string
	SyncedHash  string
	Assets      map[string]MirroredAsset
	LastError   string
	// RemoteGen is the remote's location generation the work was done for
	// (Remote.LocationGen): SaveReleaseMirror records nothing when the
	// remote has been pointed elsewhere since.
	RemoteGen   int64
	Failures    int
	NextRetryAt int64
	UpdatedAt   int64
}

const releaseCols = `id, repo_id, tag, name, body, prerelease, published, adopt, created_at, updated_at`

func scanRelease(sc interface{ Scan(...any) error }) (*Release, error) {
	var r Release
	var pre, pub, adopt int
	if err := sc.Scan(&r.ID, &r.RepoID, &r.Tag, &r.Name, &r.Body, &pre, &pub, &adopt, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, wrapErr(err)
	}
	r.Prerelease, r.Published, r.Adopt = pre != 0, pub != 0, adopt != 0
	return &r, nil
}

func (s *Store) CreateRelease(ctx context.Context, r *Release) error {
	r.CreatedAt = Now()
	r.UpdatedAt = r.CreatedAt
	res, err := s.db.ExecContext(ctx, `INSERT INTO releases (repo_id, tag, name, body, prerelease, published, adopt, created_at, updated_at)
VALUES (?,?,?,?,?,?,?,?,?)`, r.RepoID, r.Tag, r.Name, r.Body, b2i(r.Prerelease), b2i(r.Published), b2i(r.Adopt), r.CreatedAt, r.UpdatedAt)
	if err != nil {
		return wrapErr(err)
	}
	r.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) UpdateRelease(ctx context.Context, r *Release) error {
	r.UpdatedAt = Now()
	_, err := s.db.ExecContext(ctx, `UPDATE releases SET name=?, body=?, prerelease=?, published=?, adopt=?, updated_at=? WHERE id=?`,
		r.Name, r.Body, b2i(r.Prerelease), b2i(r.Published), b2i(r.Adopt), r.UpdatedAt, r.ID)
	return wrapErr(err)
}

func (s *Store) GetRelease(ctx context.Context, id int64) (*Release, error) {
	return scanRelease(s.db.QueryRowContext(ctx, `SELECT `+releaseCols+` FROM releases WHERE id=?`, id))
}

func (s *Store) GetReleaseByTag(ctx context.Context, repoID int64, tag string) (*Release, error) {
	return scanRelease(s.db.QueryRowContext(ctx, `SELECT `+releaseCols+` FROM releases WHERE repo_id=? AND tag=?`, repoID, tag))
}

// ListReleases returns a repository's releases, newest first.
func (s *Store) ListReleases(ctx context.Context, repoID int64) ([]*Release, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+releaseCols+` FROM releases WHERE repo_id=? ORDER BY created_at DESC, id DESC`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Release
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) DeleteRelease(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM releases WHERE id=?`, id)
	return wrapErr(err)
}

// TouchRelease marks a release as changed (e.g. after an asset change).
func (s *Store) TouchRelease(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE releases SET updated_at=? WHERE id=?`, Now(), id)
	return wrapErr(err)
}

// ---------------------------------------------------------------- assets

// PutReleaseAsset adds or replaces the asset with a.Name.
func (s *Store) PutReleaseAsset(ctx context.Context, a *ReleaseAsset) error {
	a.CreatedAt = Now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO release_assets (release_id, name, size, sha256, content_type, created_at) VALUES (?,?,?,?,?,?)
ON CONFLICT (release_id, name) DO UPDATE SET size=excluded.size, sha256=excluded.sha256, content_type=excluded.content_type, created_at=excluded.created_at`,
		a.ReleaseID, a.Name, a.Size, a.SHA256, a.ContentType, a.CreatedAt)
	return wrapErr(err)
}

func (s *Store) ListReleaseAssets(ctx context.Context, releaseID int64) ([]*ReleaseAsset, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, release_id, name, size, sha256, content_type, created_at FROM release_assets WHERE release_id=? ORDER BY name`, releaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ReleaseAsset
	for rows.Next() {
		var a ReleaseAsset
		if err := rows.Scan(&a.ID, &a.ReleaseID, &a.Name, &a.Size, &a.SHA256, &a.ContentType, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

func (s *Store) DeleteReleaseAsset(ctx context.Context, releaseID int64, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM release_assets WHERE release_id=? AND name=?`, releaseID, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ReleaseBlobInUse reports whether any asset of the repository's releases
// still stores content with this hash.
func (s *Store) ReleaseBlobInUse(ctx context.Context, repoID int64, sha string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM release_assets a JOIN releases r ON r.id = a.release_id WHERE r.repo_id=? AND a.sha256=?`, repoID, sha).Scan(&n)
	return n > 0, err
}

// ---------------------------------------------------------------- mirrors

func (s *Store) GetReleaseMirror(ctx context.Context, releaseID, remoteID int64) (*ReleaseMirror, error) {
	return scanReleaseMirror(s.db.QueryRowContext(ctx, `SELECT `+relMirrorCols+` FROM release_mirrors WHERE release_id=? AND remote_id=?`, releaseID, remoteID))
}

func (s *Store) ListReleaseMirrors(ctx context.Context, releaseID int64) ([]*ReleaseMirror, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+relMirrorCols+` FROM release_mirrors WHERE release_id=? ORDER BY remote_id`, releaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ReleaseMirror
	for rows.Next() {
		m, err := scanReleaseMirror(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SaveReleaseMirror inserts or replaces a release's state on one remote.
func (s *Store) SaveReleaseMirror(ctx context.Context, m *ReleaseMirror) error {
	m.UpdatedAt = Now()
	if m.Assets == nil {
		m.Assets = map[string]MirroredAsset{}
	}
	assets, _ := json.Marshal(m.Assets)
	_, err := s.db.ExecContext(ctx, `INSERT INTO release_mirrors (release_id, remote_id, remote_release, state, synced_hash, assets, last_error, failures, next_retry_at, updated_at, adopted, creating, attempt_hash)
SELECT ?,?,?,?,?,?,?,?,?,?,?,?,? WHERE (SELECT location_gen FROM remotes WHERE id=?) = ?
ON CONFLICT (release_id, remote_id) DO UPDATE SET remote_release=excluded.remote_release, state=excluded.state, synced_hash=excluded.synced_hash,
assets=excluded.assets, last_error=excluded.last_error, failures=excluded.failures, next_retry_at=excluded.next_retry_at, updated_at=excluded.updated_at,
adopted=excluded.adopted, creating=excluded.creating, attempt_hash=excluded.attempt_hash`,
		m.ReleaseID, m.RemoteID, m.RemoteRelease, m.State, m.SyncedHash, string(assets), m.LastError, m.Failures, m.NextRetryAt, m.UpdatedAt, b2i(m.Adopted), b2i(m.Creating), m.AttemptHash,
		m.RemoteID, m.RemoteGen)
	return wrapErr(err)
}

func (s *Store) DeleteReleaseMirror(ctx context.Context, releaseID, remoteID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM release_mirrors WHERE release_id=? AND remote_id=?`, releaseID, remoteID)
	return err
}

// ReleasesToRetry lists published releases with a mirror whose retry is due,
// that waits for its tag, or that has never been published to (e.g. a new
// mirror).
func (s *Store) ReleasesToRetry(ctx context.Context, now int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.id FROM releases r WHERE r.published = 1 AND (
  EXISTS (SELECT 1 FROM release_mirrors m WHERE m.release_id = r.id
          AND ((m.state IN ('failed','pending') AND m.next_retry_at <= ?) OR m.state = 'waiting'))
  OR EXISTS (SELECT 1 FROM remotes x WHERE x.repo_id = r.repo_id AND x.enabled = 1
          AND NOT EXISTS (SELECT 1 FROM release_mirrors m2 WHERE m2.release_id = r.id AND m2.remote_id = x.id)))`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

const relMirrorCols = `release_id, remote_id, remote_release, state, synced_hash, assets, last_error, failures, next_retry_at, updated_at, adopted, creating, attempt_hash`

func scanReleaseMirror(sc interface{ Scan(...any) error }) (*ReleaseMirror, error) {
	var m ReleaseMirror
	var assets string
	var adopted, creating int
	if err := sc.Scan(&m.ReleaseID, &m.RemoteID, &m.RemoteRelease, &m.State, &m.SyncedHash, &assets, &m.LastError, &m.Failures, &m.NextRetryAt, &m.UpdatedAt, &adopted, &creating, &m.AttemptHash); err != nil {
		return nil, wrapErr(err)
	}
	m.Adopted, m.Creating = adopted != 0, creating != 0
	m.Assets = map[string]MirroredAsset{}
	_ = json.Unmarshal([]byte(assets), &m.Assets)
	return &m, nil
}

// ReleaseAssetsOfRepo lists the assets of all releases of a repository.
func (s *Store) ReleaseAssetsOfRepo(ctx context.Context, repoID int64) ([]*ReleaseAsset, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.id, a.release_id, a.name, a.size, a.sha256, a.content_type, a.created_at
FROM release_assets a JOIN releases r ON r.id = a.release_id WHERE r.repo_id = ? ORDER BY a.id`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ReleaseAsset
	for rows.Next() {
		var a ReleaseAsset
		if err := rows.Scan(&a.ID, &a.ReleaseID, &a.Name, &a.Size, &a.SHA256, &a.ContentType, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}
