package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Remote states.
const (
	StatePending  = "pending"
	StateSynced   = "synced"
	StateSyncing  = "syncing"
	StateBehind   = "behind"
	StateDiverged = "diverged"
	StateFailed   = "failed"
	StatePaused   = "paused"
)

// Divergence policies.
const (
	OnDivergenceAlert     = "alert"
	OnDivergenceOverwrite = "overwrite"
)

// Target is a platform account: provider + credential + default namespace.
// Repositories mirror to targets through remotes.
type Target struct {
	ID            int64
	Name          string
	Provider      string // github | gitlab | generic
	BaseURL       string
	APIURL        string
	Namespace     string
	URLTemplate   string
	Visibility    string // "" follows the repository
	AutoCreate    bool
	MaxFileSize   int64
	CredKind      string // token | basic | ssh | none
	CredUsername  string
	CredSecret    []byte // ciphertext
	SSHKnownHosts string
	ExpiresAt     int64
	CreatedAt     int64
	UpdatedAt     int64
}

// CredentialAAD binds a target's ciphertext to the target.
func (t *Target) CredentialAAD() string { return "target:" + t.Name }

type Binding struct {
	ID           int64
	Org          string // org name or "*"
	TargetID     int64
	TargetName   string
	Namespace    string
	OnDivergence string
	// NameTemplate names the mirror repository; {org} and {name} are the
	// hub repository's org and name.
	NameTemplate string
	CreatedAt    int64
}

// MirrorName applies the binding's name template.
func (b *Binding) MirrorName(org, name string) string {
	t := b.NameTemplate
	if t == "" {
		t = "{name}"
	}
	return strings.NewReplacer("{org}", org, "{name}", name).Replace(t)
}

type Repo struct {
	ID            int64
	Slug          string
	DefaultBranch string
	Visibility    string
	CreatedAt     int64
	UpdatedAt     int64
}

type Remote struct {
	ID                int64
	RepoID            int64
	TargetID          int64
	TargetName        string
	TargetNamespace   string
	Provider          string
	Namespace         string
	Name              string
	URL               string
	OnDivergence      string
	Enabled           bool
	Provisioned       bool
	AtomicUnsupported bool
	// Adopt allows writing into a mirror repository that already holds refs
	// repohub did not create.
	Adopt               bool
	State               string
	LastError           string
	ConsecutiveFailures int
	NextRetryAt         int64
	LastSyncAt          int64
	LastSuccessAt       int64
	CreatedAt           int64
}

// EffectiveNamespace is the remote's namespace, falling back to the target's.
func (r *Remote) EffectiveNamespace() string {
	if r.Namespace != "" {
		return r.Namespace
	}
	return r.TargetNamespace
}

// Label is a human-readable identifier such as "github:acme/demo".
func (r *Remote) Label() string {
	if ns := r.EffectiveNamespace(); ns != "" {
		return r.TargetName + ":" + ns + "/" + r.Name
	}
	return r.TargetName + ":" + r.Name
}

type Divergence struct {
	RemoteID    int64
	Ref         string
	HubSHA      string
	RemoteSHA   string
	ExpectedSHA string
	Reason      string
	DetectedAt  int64
}

type SyncRun struct {
	ID          int64
	RepoID      int64
	RemoteID    int64
	RemoteLabel string
	Trigger     string
	Status      string
	RefsChanged int
	Error       string
	Detail      string
	StartedAt   int64
	FinishedAt  int64
}

type Token struct {
	ID         int64
	Name       string
	Hash       string
	Scopes     []string
	Repos      []string
	CreatedAt  int64
	LastUsedAt int64
}

// ---------------------------------------------------------------- targets

const targetCols = `id, name, provider, base_url, api_url, namespace, url_template, visibility,
auto_create, max_file_size, cred_kind, cred_username, cred_secret, ssh_known_hosts, expires_at, created_at, updated_at`

func scanTarget(sc interface{ Scan(...any) error }) (*Target, error) {
	var t Target
	var auto int
	err := sc.Scan(&t.ID, &t.Name, &t.Provider, &t.BaseURL, &t.APIURL, &t.Namespace, &t.URLTemplate, &t.Visibility,
		&auto, &t.MaxFileSize, &t.CredKind, &t.CredUsername, &t.CredSecret, &t.SSHKnownHosts, &t.ExpiresAt, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, wrapErr(err)
	}
	t.AutoCreate = auto != 0
	return &t, nil
}

func (s *Store) CreateTarget(ctx context.Context, t *Target) error {
	now := Now()
	t.CreatedAt, t.UpdatedAt = now, now
	res, err := s.db.ExecContext(ctx, `INSERT INTO targets (name, provider, base_url, api_url, namespace, url_template, visibility,
auto_create, max_file_size, cred_kind, cred_username, cred_secret, ssh_known_hosts, expires_at, created_at, updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.Name, t.Provider, t.BaseURL, t.APIURL, t.Namespace, t.URLTemplate, t.Visibility,
		b2i(t.AutoCreate), t.MaxFileSize, t.CredKind, t.CredUsername, t.CredSecret, t.SSHKnownHosts, t.ExpiresAt, t.CreatedAt, t.UpdatedAt)
	if err != nil {
		return wrapErr(err)
	}
	t.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) UpdateTarget(ctx context.Context, t *Target) error {
	t.UpdatedAt = Now()
	_, err := s.db.ExecContext(ctx, `UPDATE targets SET provider=?, base_url=?, api_url=?, namespace=?, url_template=?, visibility=?,
auto_create=?, max_file_size=?, cred_kind=?, cred_username=?, cred_secret=?, ssh_known_hosts=?, expires_at=?, updated_at=? WHERE id=?`,
		t.Provider, t.BaseURL, t.APIURL, t.Namespace, t.URLTemplate, t.Visibility,
		b2i(t.AutoCreate), t.MaxFileSize, t.CredKind, t.CredUsername, t.CredSecret, t.SSHKnownHosts, t.ExpiresAt, t.UpdatedAt, t.ID)
	return wrapErr(err)
}

func (s *Store) GetTarget(ctx context.Context, id int64) (*Target, error) {
	return scanTarget(s.db.QueryRowContext(ctx, `SELECT `+targetCols+` FROM targets WHERE id=?`, id))
}

func (s *Store) GetTargetByName(ctx context.Context, name string) (*Target, error) {
	return scanTarget(s.db.QueryRowContext(ctx, `SELECT `+targetCols+` FROM targets WHERE name=?`, name))
}

func (s *Store) ListTargets(ctx context.Context) ([]*Target, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+targetCols+` FROM targets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Target
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteTarget removes a target. It fails with a descriptive error while
// remotes still reference it.
func (s *Store) DeleteTarget(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM targets WHERE id=?`, id)
	if isForeignKey(err) {
		return errors.New("target is still used by remotes; remove those remotes first")
	}
	return wrapErr(err)
}

// --------------------------------------------------------------- bindings

func (s *Store) CreateBinding(ctx context.Context, b *Binding) error {
	b.CreatedAt = Now()
	if b.NameTemplate == "" {
		b.NameTemplate = "{name}"
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO bindings (org, target_id, namespace, on_divergence, name_template, created_at) VALUES (?,?,?,?,?,?)`,
		b.Org, b.TargetID, b.Namespace, b.OnDivergence, b.NameTemplate, b.CreatedAt)
	if err != nil {
		return wrapErr(err)
	}
	b.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) DeleteBinding(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM bindings WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListBindings returns bindings, optionally only those applying to org
// (exact match or "*").
func (s *Store) ListBindings(ctx context.Context, org string) ([]*Binding, error) {
	q := `SELECT b.id, b.org, b.target_id, t.name, b.namespace, b.on_divergence, b.name_template, b.created_at
FROM bindings b JOIN targets t ON t.id = b.target_id`
	var args []any
	if org != "" {
		q += ` WHERE b.org = ? OR b.org = '*'`
		args = append(args, org)
	}
	q += ` ORDER BY b.org, t.name`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Binding
	for rows.Next() {
		var b Binding
		if err := rows.Scan(&b.ID, &b.Org, &b.TargetID, &b.TargetName, &b.Namespace, &b.OnDivergence, &b.NameTemplate, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &b)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------ repos

const repoCols = `id, slug, default_branch, visibility, created_at, updated_at`

func scanRepo(sc interface{ Scan(...any) error }) (*Repo, error) {
	var r Repo
	if err := sc.Scan(&r.ID, &r.Slug, &r.DefaultBranch, &r.Visibility, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &r, nil
}

func (s *Store) CreateRepo(ctx context.Context, r *Repo) error {
	now := Now()
	r.CreatedAt, r.UpdatedAt = now, now
	res, err := s.db.ExecContext(ctx, `INSERT INTO repos (slug, default_branch, visibility, created_at, updated_at) VALUES (?,?,?,?,?)`,
		r.Slug, r.DefaultBranch, r.Visibility, r.CreatedAt, r.UpdatedAt)
	if err != nil {
		return wrapErr(err)
	}
	r.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) GetRepo(ctx context.Context, id int64) (*Repo, error) {
	return scanRepo(s.db.QueryRowContext(ctx, `SELECT `+repoCols+` FROM repos WHERE id=?`, id))
}

func (s *Store) GetRepoBySlug(ctx context.Context, slug string) (*Repo, error) {
	return scanRepo(s.db.QueryRowContext(ctx, `SELECT `+repoCols+` FROM repos WHERE slug=?`, slug))
}

func (s *Store) ListRepos(ctx context.Context) ([]*Repo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+repoCols+` FROM repos ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) UpdateRepo(ctx context.Context, r *Repo) error {
	r.UpdatedAt = Now()
	_, err := s.db.ExecContext(ctx, `UPDATE repos SET default_branch=?, visibility=?, updated_at=? WHERE id=?`,
		r.DefaultBranch, r.Visibility, r.UpdatedAt, r.ID)
	return wrapErr(err)
}

func (s *Store) DeleteRepo(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM repos WHERE id=?`, id)
	return wrapErr(err)
}

// ---------------------------------------------------------------- remotes

const remoteCols = `r.id, r.repo_id, r.target_id, t.name, t.namespace, t.provider, r.namespace, r.name, r.url, r.on_divergence, r.enabled,
r.provisioned, r.atomic_unsupported, r.adopt, r.state, r.last_error, r.consecutive_failures, r.next_retry_at, r.last_sync_at,
r.last_success_at, r.created_at`

const remoteFrom = ` FROM remotes r JOIN targets t ON t.id = r.target_id`

func scanRemote(sc interface{ Scan(...any) error }) (*Remote, error) {
	var r Remote
	var enabled, prov, atomic, adopt int
	err := sc.Scan(&r.ID, &r.RepoID, &r.TargetID, &r.TargetName, &r.TargetNamespace, &r.Provider, &r.Namespace, &r.Name, &r.URL, &r.OnDivergence, &enabled,
		&prov, &atomic, &adopt, &r.State, &r.LastError, &r.ConsecutiveFailures, &r.NextRetryAt, &r.LastSyncAt,
		&r.LastSuccessAt, &r.CreatedAt)
	if err != nil {
		return nil, wrapErr(err)
	}
	r.Enabled, r.Provisioned, r.AtomicUnsupported, r.Adopt = enabled != 0, prov != 0, atomic != 0, adopt != 0
	return &r, nil
}

func (s *Store) CreateRemote(ctx context.Context, r *Remote) error {
	r.CreatedAt = Now()
	if r.State == "" {
		r.State = StatePending
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO remotes (repo_id, target_id, namespace, name, url, on_divergence, enabled, adopt, state, created_at)
VALUES (?,?,?,?,?,?,?,?,?,?)`, r.RepoID, r.TargetID, r.Namespace, r.Name, r.URL, r.OnDivergence, b2i(r.Enabled), b2i(r.Adopt), r.State, r.CreatedAt)
	if err != nil {
		return wrapErr(err)
	}
	r.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) GetRemote(ctx context.Context, id int64) (*Remote, error) {
	return scanRemote(s.db.QueryRowContext(ctx, `SELECT `+remoteCols+remoteFrom+` WHERE r.id=?`, id))
}

func (s *Store) ListRemotes(ctx context.Context, repoID int64) ([]*Remote, error) {
	return s.queryRemotes(ctx, `SELECT `+remoteCols+remoteFrom+` WHERE r.repo_id=? ORDER BY t.name, r.namespace, r.name`, repoID)
}

func (s *Store) ListAllRemotes(ctx context.Context) ([]*Remote, error) {
	return s.queryRemotes(ctx, `SELECT `+remoteCols+remoteFrom+` ORDER BY r.repo_id, t.name`)
}

// DueRetries returns remotes whose backoff has elapsed.
func (s *Store) DueRetries(ctx context.Context, now int64) ([]*Remote, error) {
	return s.queryRemotes(ctx, `SELECT `+remoteCols+remoteFrom+` WHERE r.next_retry_at > 0 AND r.next_retry_at <= ? AND r.enabled = 1`, now)
}

func (s *Store) queryRemotes(ctx context.Context, q string, args ...any) ([]*Remote, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Remote
	for rows.Next() {
		r, err := scanRemote(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) UpdateRemoteConfig(ctx context.Context, r *Remote) error {
	_, err := s.db.ExecContext(ctx, `UPDATE remotes SET url=?, on_divergence=?, enabled=?, adopt=? WHERE id=?`, r.URL, r.OnDivergence, b2i(r.Enabled), b2i(r.Adopt), r.ID)
	return wrapErr(err)
}

// RemoteUsingLocation returns a remote of another repository that already
// mirrors to the same place: the same explicit URL, or the same target,
// effective namespace and name (compared case-insensitively, as platforms
// do). It returns nil when the location is free.
func (s *Store) RemoteUsingLocation(ctx context.Context, repoID, targetID int64, namespace, name, url string) (*Remote, error) {
	var rows []*Remote
	var err error
	if url != "" {
		rows, err = s.queryRemotes(ctx, `SELECT `+remoteCols+remoteFrom+` WHERE r.url = ? AND r.repo_id != ? LIMIT 1`, url, repoID)
	} else {
		rows, err = s.queryRemotes(ctx, `SELECT `+remoteCols+remoteFrom+` WHERE r.url = '' AND r.target_id = ? AND r.repo_id != ?
AND lower(COALESCE(NULLIF(r.namespace, ''), t.namespace)) = lower(COALESCE(NULLIF(?, ''), t.namespace)) AND lower(r.name) = lower(?) LIMIT 1`,
			targetID, repoID, namespace, name)
	}
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

func (s *Store) DeleteRemote(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM remotes WHERE id=?`, id)
	return wrapErr(err)
}

func (s *Store) SetRemoteProvisioned(ctx context.Context, id int64, v bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE remotes SET provisioned=? WHERE id=?`, b2i(v), id)
	return err
}

func (s *Store) SetRemoteAtomicUnsupported(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE remotes SET atomic_unsupported=1 WHERE id=?`, id)
	return err
}

func (s *Store) SetRemoteState(ctx context.Context, id int64, state string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE remotes SET state=? WHERE id=?`, state, id)
	return err
}

// MarkSyncing flags a remote as being synced and clears its pending retry.
func (s *Store) MarkSyncing(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE remotes SET state=?, next_retry_at=0 WHERE id=?`, StateSyncing, id)
	return err
}

// RemoteResult is the outcome of one sync attempt.
type RemoteResult struct {
	State       string
	Error       string
	SyncedAt    int64
	Success     bool // all refs equal the hub
	Failed      bool // counts toward consecutive failures
	NextRetryAt int64
}

func (s *Store) RecordRemoteResult(ctx context.Context, id int64, res RemoteResult) error {
	q := `UPDATE remotes SET state=?, last_error=?, last_sync_at=?, next_retry_at=?`
	args := []any{res.State, res.Error, res.SyncedAt, res.NextRetryAt}
	if res.Success {
		q += `, last_success_at=?`
		args = append(args, res.SyncedAt)
	}
	if res.Failed {
		q += `, consecutive_failures = consecutive_failures + 1`
	} else {
		q += `, consecutive_failures = 0`
	}
	q += ` WHERE id=?`
	args = append(args, id)
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

// ------------------------------------------------- ref / pushed state

// ReplaceRefState records the refs observed on a remote.
func (s *Store) ReplaceRefState(ctx context.Context, remoteID int64, refs map[string]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM ref_state WHERE remote_id=?`, remoteID); err != nil {
		return err
	}
	now := Now()
	for ref, sha := range refs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ref_state (remote_id, ref, sha, observed_at) VALUES (?,?,?,?)`, remoteID, ref, sha, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) GetRefState(ctx context.Context, remoteID int64) (map[string]string, error) {
	return s.refMap(ctx, `SELECT ref, sha FROM ref_state WHERE remote_id=?`, remoteID)
}

func (s *Store) GetPushedState(ctx context.Context, remoteID int64) (map[string]string, error) {
	return s.refMap(ctx, `SELECT ref, sha FROM pushed_state WHERE remote_id=?`, remoteID)
}

func (s *Store) refMap(ctx context.Context, q string, args ...any) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var ref, sha string
		if err := rows.Scan(&ref, &sha); err != nil {
			return nil, err
		}
		out[ref] = sha
	}
	return out, rows.Err()
}

// ApplyPushedState upserts and deletes pushed_state entries atomically.
func (s *Store) ApplyPushedState(ctx context.Context, remoteID int64, set map[string]string, del []string) error {
	if len(set) == 0 && len(del) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := Now()
	for ref, sha := range set {
		if _, err := tx.ExecContext(ctx, `INSERT INTO pushed_state (remote_id, ref, sha, pushed_at) VALUES (?,?,?,?)
ON CONFLICT (remote_id, ref) DO UPDATE SET sha=excluded.sha, pushed_at=excluded.pushed_at`, remoteID, ref, sha, now); err != nil {
			return err
		}
	}
	for _, ref := range del {
		if _, err := tx.ExecContext(ctx, `DELETE FROM pushed_state WHERE remote_id=? AND ref=?`, remoteID, ref); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ------------------------------------------------------------ divergences

func (s *Store) ReplaceDivergences(ctx context.Context, remoteID int64, divs []Divergence) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Preserve the original detection time for divergences that persist.
	existing := map[string]int64{}
	rows, err := tx.QueryContext(ctx, `SELECT ref, detected_at FROM divergences WHERE remote_id=?`, remoteID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var ref string
		var at int64
		if err := rows.Scan(&ref, &at); err != nil {
			rows.Close()
			return err
		}
		existing[ref] = at
	}
	rows.Close()
	if _, err := tx.ExecContext(ctx, `DELETE FROM divergences WHERE remote_id=?`, remoteID); err != nil {
		return err
	}
	now := Now()
	for _, d := range divs {
		at := now
		if prev, ok := existing[d.Ref]; ok {
			at = prev
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO divergences (remote_id, ref, hub_sha, remote_sha, expected_sha, reason, detected_at)
VALUES (?,?,?,?,?,?,?)`, remoteID, d.Ref, d.HubSHA, d.RemoteSHA, d.ExpectedSHA, d.Reason, at); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListDivergences(ctx context.Context, remoteID int64) ([]Divergence, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT remote_id, ref, hub_sha, remote_sha, expected_sha, reason, detected_at
FROM divergences WHERE remote_id=? ORDER BY ref`, remoteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Divergence
	for rows.Next() {
		var d Divergence
		if err := rows.Scan(&d.RemoteID, &d.Ref, &d.HubSHA, &d.RemoteSHA, &d.ExpectedSHA, &d.Reason, &d.DetectedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// -------------------------------------------------------------- sync runs

func (s *Store) InsertRun(ctx context.Context, r *SyncRun) error {
	r.StartedAt = Now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO sync_runs (repo_id, remote_id, trigger, status, started_at) VALUES (?,?,?,?,?)`,
		r.RepoID, r.RemoteID, r.Trigger, r.Status, r.StartedAt)
	if err != nil {
		return err
	}
	r.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) FinishRun(ctx context.Context, r *SyncRun) error {
	r.FinishedAt = Now()
	_, err := s.db.ExecContext(ctx, `UPDATE sync_runs SET status=?, refs_changed=?, error=?, detail=?, finished_at=? WHERE id=?`,
		r.Status, r.RefsChanged, r.Error, r.Detail, r.FinishedAt, r.ID)
	return err
}

func (s *Store) ListRuns(ctx context.Context, repoID int64, limit int) ([]*SyncRun, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT s.id, s.repo_id, s.remote_id,
t.name || ':' || CASE WHEN COALESCE(NULLIF(r.namespace, ''), t.namespace) = '' THEN r.name ELSE COALESCE(NULLIF(r.namespace, ''), t.namespace) || '/' || r.name END,
s.trigger, s.status, s.refs_changed, s.error, s.detail, s.started_at, s.finished_at
FROM sync_runs s JOIN remotes r ON r.id = s.remote_id JOIN targets t ON t.id = r.target_id
WHERE s.repo_id=? ORDER BY s.id DESC LIMIT ?`, repoID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*SyncRun
	for rows.Next() {
		var r SyncRun
		if err := rows.Scan(&r.ID, &r.RepoID, &r.RemoteID, &r.RemoteLabel, &r.Trigger, &r.Status, &r.RefsChanged, &r.Error, &r.Detail, &r.StartedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// PruneRuns keeps the most recent `keep` runs per remote.
func (s *Store) PruneRuns(ctx context.Context, keep int) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sync_runs WHERE id IN (
SELECT id FROM (SELECT id, ROW_NUMBER() OVER (PARTITION BY remote_id ORDER BY id DESC) AS rn FROM sync_runs) WHERE rn > ?)`, keep)
	return err
}

// ----------------------------------------------------------------- tokens

func (s *Store) CreateToken(ctx context.Context, t *Token) error {
	t.CreatedAt = Now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO tokens (name, token_hash, scopes, repos, created_at) VALUES (?,?,?,?,?)`,
		t.Name, t.Hash, strings.Join(t.Scopes, ","), strings.Join(t.Repos, ","), t.CreatedAt)
	if err != nil {
		return wrapErr(err)
	}
	t.ID, _ = res.LastInsertId()
	return nil
}

func scanToken(sc interface{ Scan(...any) error }) (*Token, error) {
	var t Token
	var scopes, repos string
	if err := sc.Scan(&t.ID, &t.Name, &t.Hash, &scopes, &repos, &t.CreatedAt, &t.LastUsedAt); err != nil {
		return nil, wrapErr(err)
	}
	t.Scopes = splitList(scopes)
	t.Repos = splitList(repos)
	return &t, nil
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func (s *Store) GetTokenByHash(ctx context.Context, hash string) (*Token, error) {
	return scanToken(s.db.QueryRowContext(ctx, `SELECT id, name, token_hash, scopes, repos, created_at, last_used_at FROM tokens WHERE token_hash=?`, hash))
}

func (s *Store) ListTokens(ctx context.Context) ([]*Token, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, token_hash, scopes, repos, created_at, last_used_at FROM tokens ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteToken(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) TouchToken(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tokens SET last_used_at=? WHERE id=?`, Now(), id)
	return err
}

// ----------------------------------------------------------------- leases

// AcquireLease takes or renews the named lease for holder. It returns false
// when another holder owns an unexpired lease.
func (s *Store) AcquireLease(ctx context.Context, name, holder string, ttlMillis int64) (bool, error) {
	now := Now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO leases (name, holder, expires_at) VALUES (?,?,?)
ON CONFLICT (name) DO UPDATE SET holder=excluded.holder, expires_at=excluded.expires_at
WHERE leases.holder = excluded.holder OR leases.expires_at < ?`, name, holder, now+ttlMillis, now)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) ReleaseLease(ctx context.Context, name, holder string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM leases WHERE name=? AND holder=?`, name, holder)
	return err
}

// Ping checks database connectivity.
func (s *Store) Ping(ctx context.Context) error {
	var one int
	if err := s.db.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		return fmt.Errorf("db: %w", err)
	}
	return nil
}
