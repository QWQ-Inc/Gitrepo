package store

import (
	"context"
	"strings"
)

// SSHKey is a public key that grants Git access over SSH with the same
// scopes and repository patterns as a token.
type SSHKey struct {
	ID          int64
	Name        string
	Fingerprint string // SHA256:… as printed by ssh-keygen -l
	PublicKey   string // authorized_keys line
	Scopes      []string
	Repos       []string
	CreatedAt   int64
	LastUsedAt  int64
}

const sshKeyCols = `id, name, fingerprint, public_key, scopes, repos, created_at, last_used_at`

func scanSSHKey(sc interface{ Scan(...any) error }) (*SSHKey, error) {
	var k SSHKey
	var scopes, repos string
	if err := sc.Scan(&k.ID, &k.Name, &k.Fingerprint, &k.PublicKey, &scopes, &repos, &k.CreatedAt, &k.LastUsedAt); err != nil {
		return nil, wrapErr(err)
	}
	k.Scopes, k.Repos = splitList(scopes), splitList(repos)
	return &k, nil
}

func (s *Store) CreateSSHKey(ctx context.Context, k *SSHKey) error {
	k.CreatedAt = Now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO ssh_keys (name, fingerprint, public_key, scopes, repos, created_at) VALUES (?,?,?,?,?,?)`,
		k.Name, k.Fingerprint, k.PublicKey, strings.Join(k.Scopes, ","), strings.Join(k.Repos, ","), k.CreatedAt)
	if err != nil {
		return wrapErr(err)
	}
	k.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) GetSSHKeyByFingerprint(ctx context.Context, fp string) (*SSHKey, error) {
	return scanSSHKey(s.db.QueryRowContext(ctx, `SELECT `+sshKeyCols+` FROM ssh_keys WHERE fingerprint=?`, fp))
}

func (s *Store) GetSSHKey(ctx context.Context, id int64) (*SSHKey, error) {
	return scanSSHKey(s.db.QueryRowContext(ctx, `SELECT `+sshKeyCols+` FROM ssh_keys WHERE id=?`, id))
}

func (s *Store) ListSSHKeys(ctx context.Context) ([]*SSHKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sshKeyCols+` FROM ssh_keys ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*SSHKey
	for rows.Next() {
		k, err := scanSSHKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) DeleteSSHKey(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM ssh_keys WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) TouchSSHKey(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE ssh_keys SET last_used_at=? WHERE id=?`, Now(), id)
	return err
}
