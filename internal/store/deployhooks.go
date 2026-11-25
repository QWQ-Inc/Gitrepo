package store

import (
	"context"
	"database/sql"
	"strconv"
)

// DeployHook tells a deployment platform that a branch has a new commit:
// on the hub as soon as it is pushed, or on one mirror once the sync has
// put it there (for a platform that builds from that mirror).
type DeployHook struct {
	ID      int64
	RepoID  int64
	Name    string
	Kind    string // "generic" or a preset ("stackenigma")
	URL     []byte // sealed
	URLHost string
	Secret  []byte // sealed signing secret (optional)
	Token   []byte // sealed bearer token (optional)
	// Branch is the branch's name on the source ("" = the repository's
	// default branch).
	Branch string
	// RemoteID is the mirror the platform builds from; 0 = the hub.
	RemoteID   int64
	Enabled    bool
	LastStatus string
	LastCommit string
	LastAt     int64
	CreatedAt  int64
}

// AAD binds a deploy hook's ciphertexts to it.
func (h *DeployHook) AAD(field string) string {
	return "deploy:" + strconv.FormatInt(h.RepoID, 10) + ":" + h.Name + ":" + field
}

const deployCols = `id, repo_id, name, kind, url, url_host, secret, token, branch, remote_id, enabled, last_status, last_commit, last_at, created_at`

func scanDeployHook(sc interface{ Scan(...any) error }) (*DeployHook, error) {
	var h DeployHook
	var remote sql.NullInt64
	var enabled int
	if err := sc.Scan(&h.ID, &h.RepoID, &h.Name, &h.Kind, &h.URL, &h.URLHost, &h.Secret, &h.Token, &h.Branch, &remote,
		&enabled, &h.LastStatus, &h.LastCommit, &h.LastAt, &h.CreatedAt); err != nil {
		return nil, wrapErr(err)
	}
	h.RemoteID, h.Enabled = remote.Int64, enabled != 0
	return &h, nil
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func (s *Store) CreateDeployHook(ctx context.Context, h *DeployHook) error {
	h.CreatedAt = Now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO deploy_hooks (repo_id, name, kind, url, url_host, secret, token, branch, remote_id, enabled, created_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?)`, h.RepoID, h.Name, h.Kind, h.URL, h.URLHost, h.Secret, h.Token, h.Branch, nullID(h.RemoteID), b2i(h.Enabled), h.CreatedAt)
	if err != nil {
		return wrapErr(err)
	}
	h.ID, _ = res.LastInsertId()
	return nil
}

// UpdateDeployHook saves the hook's settings (not its delivery state).
func (s *Store) UpdateDeployHook(ctx context.Context, h *DeployHook) error {
	res, err := s.db.ExecContext(ctx, `UPDATE deploy_hooks SET kind=?, url=?, url_host=?, secret=?, token=?, branch=?, remote_id=?, enabled=? WHERE id=?`,
		h.Kind, h.URL, h.URLHost, h.Secret, h.Token, h.Branch, nullID(h.RemoteID), b2i(h.Enabled), h.ID)
	if err != nil {
		return wrapErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetDeployHook(ctx context.Context, id int64) (*DeployHook, error) {
	return scanDeployHook(s.db.QueryRowContext(ctx, `SELECT `+deployCols+` FROM deploy_hooks WHERE id=?`, id))
}

// ListDeployHooks returns a repository's deploy hooks.
func (s *Store) ListDeployHooks(ctx context.Context, repoID int64) ([]*DeployHook, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deployCols+` FROM deploy_hooks WHERE repo_id=? ORDER BY name`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*DeployHook
	for rows.Next() {
		h, err := scanDeployHook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) DeleteDeployHook(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM deploy_hooks WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordDeployDelivery stores the outcome of a delivery. commit is recorded
// only when the delivery succeeded, so a failed one is retried by the next
// update of the branch.
func (s *Store) RecordDeployDelivery(ctx context.Context, id int64, status, commit string, ok bool) error {
	if ok {
		_, err := s.db.ExecContext(ctx, `UPDATE deploy_hooks SET last_status=?, last_commit=?, last_at=? WHERE id=?`, status, commit, Now(), id)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE deploy_hooks SET last_status=?, last_at=? WHERE id=?`, status, Now(), id)
	return err
}
