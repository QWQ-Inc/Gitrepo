package store

import (
	"context"
	"strconv"
	"strings"
)

// User is an account: a user name and password for the console and for
// git over HTTPS. Like a token it has scopes and repository patterns.
type User struct {
	ID                int64
	Username          string
	PasswordHash      string
	Scopes            []string
	Repos             []string
	Disabled          bool
	CreatedAt         int64
	UpdatedAt         int64
	PasswordChangedAt int64
	LastLoginAt       int64
}

// Subject is how sessions refer to the account.
func (u *User) Subject() string { return strconv.FormatInt(u.ID, 10) }

const userCols = `id, username, password_hash, scopes, repos, disabled, created_at, updated_at, password_changed_at, last_login_at`

func scanUser(sc interface{ Scan(...any) error }) (*User, error) {
	var u User
	var scopes, repos string
	var disabled int
	if err := sc.Scan(&u.ID, &u.Username, &u.PasswordHash, &scopes, &repos, &disabled, &u.CreatedAt, &u.UpdatedAt, &u.PasswordChangedAt, &u.LastLoginAt); err != nil {
		return nil, wrapErr(err)
	}
	u.Scopes, u.Repos, u.Disabled = splitList(scopes), splitList(repos), disabled != 0
	return &u, nil
}

// CreateFirstUser creates u only when there is no account yet (the first
// account, set up with the bootstrap admin token). It returns ErrConflict
// otherwise.
func (s *Store) CreateFirstUser(ctx context.Context, u *User) error {
	return s.createUser(ctx, u, true)
}

func (s *Store) CreateUser(ctx context.Context, u *User) error {
	return s.createUser(ctx, u, false)
}

func (s *Store) createUser(ctx context.Context, u *User, onlyFirst bool) error {
	now := Now()
	u.CreatedAt, u.UpdatedAt, u.PasswordChangedAt = now, now, now
	q := `INSERT INTO users (username, password_hash, scopes, repos, disabled, created_at, updated_at, password_changed_at) VALUES (?,?,?,?,?,?,?,?)`
	if onlyFirst {
		q = `INSERT INTO users (username, password_hash, scopes, repos, disabled, created_at, updated_at, password_changed_at)
SELECT ?,?,?,?,?,?,?,? WHERE NOT EXISTS (SELECT 1 FROM users)`
	}
	res, err := s.db.ExecContext(ctx, q, u.Username, u.PasswordHash, strings.Join(u.Scopes, ","), strings.Join(u.Repos, ","), b2i(u.Disabled), now, now, now)
	if err != nil {
		return wrapErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConflict
	}
	u.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) GetUser(ctx context.Context, username string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE username=?`, username))
}

func (s *Store) GetUserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id=?`, id))
}

func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountUsers returns the number of accounts and of enabled administrators.
func (s *Store) CountUsers(ctx context.Context) (all, admins int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN disabled=0 AND (','||scopes||',') LIKE '%,admin,%' THEN 1 ELSE 0 END), 0) FROM users`).Scan(&all, &admins)
	return all, admins, err
}

// UpdateUser saves scopes, repositories and the disabled flag, and signs
// the account's sessions out (they carry the old permissions).
func (s *Store) UpdateUser(ctx context.Context, u *User) error {
	return s.userTx(ctx, u.ID, `UPDATE users SET scopes=?, repos=?, disabled=?, updated_at=? WHERE id=?`,
		strings.Join(u.Scopes, ","), strings.Join(u.Repos, ","), b2i(u.Disabled), Now(), u.ID)
}

// SetPassword changes an account's password and signs out its sessions.
func (s *Store) SetPassword(ctx context.Context, id int64, hash string) error {
	now := Now()
	return s.userTx(ctx, id, `UPDATE users SET password_hash=?, password_changed_at=?, updated_at=? WHERE id=?`, hash, now, now, id)
}

// DeleteUser removes an account and signs out its sessions.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	return s.userTx(ctx, id, `DELETE FROM users WHERE id=?`, id)
}

func (s *Store) userTx(ctx context.Context, id int64, q string, args ...any) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return wrapErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE provider=? AND subject=?`, LocalProvider, strconv.FormatInt(id, 10)); err != nil {
		return err
	}
	return tx.Commit()
}

// LocalProvider is the sessions' provider for password sign-ins.
const LocalProvider = "local"

func (s *Store) TouchUserLogin(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET last_login_at=? WHERE id=?`, Now(), id)
	return err
}
