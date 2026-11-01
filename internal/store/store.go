// Package store persists Gitrepo state in SQLite (pure-Go driver, no cgo).
// The schema avoids SQLite-only features so it can move to Postgres later.
//
// Timestamps are stored as Unix milliseconds; 0 means "unset".
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a looked-up row does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned on unique-constraint violations.
var ErrConflict = errors.New("already exists")

type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and applies
// migrations.
func Open(path string) (*Store, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	// One connection serialises access; Gitrepo's write volume is tiny and
	// this rules out SQLITE_BUSY between goroutines. Callers must never hold
	// a *sql.Rows open while issuing another query.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Now returns the current time in the storage representation.
func Now() int64 { return time.Now().UnixMilli() }

// ToTime converts a stored timestamp; zero stays zero.
func ToTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func FromTime(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

var migrations = []string{
	// 1: initial schema
	`
CREATE TABLE targets (
  id              INTEGER PRIMARY KEY,
  name            TEXT    NOT NULL UNIQUE,
  provider        TEXT    NOT NULL,
  base_url        TEXT    NOT NULL DEFAULT '',
  api_url         TEXT    NOT NULL DEFAULT '',
  namespace       TEXT    NOT NULL DEFAULT '',
  url_template    TEXT    NOT NULL DEFAULT '',
  visibility      TEXT    NOT NULL DEFAULT '',
  auto_create     INTEGER NOT NULL DEFAULT 1,
  max_file_size   INTEGER NOT NULL DEFAULT 0,
  cred_kind       TEXT    NOT NULL DEFAULT 'none',
  cred_username   TEXT    NOT NULL DEFAULT '',
  cred_secret     BLOB,
  ssh_known_hosts TEXT    NOT NULL DEFAULT '',
  expires_at      INTEGER NOT NULL DEFAULT 0,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);
CREATE TABLE bindings (
  id            INTEGER PRIMARY KEY,
  org           TEXT    NOT NULL,
  target_id     INTEGER NOT NULL REFERENCES targets(id) ON DELETE CASCADE,
  namespace     TEXT    NOT NULL DEFAULT '',
  on_divergence TEXT    NOT NULL DEFAULT 'alert',
  created_at    INTEGER NOT NULL,
  UNIQUE (org, target_id)
);
CREATE TABLE repos (
  id             INTEGER PRIMARY KEY,
  slug           TEXT    NOT NULL UNIQUE,
  default_branch TEXT    NOT NULL DEFAULT '',
  visibility     TEXT    NOT NULL DEFAULT 'private',
  created_at     INTEGER NOT NULL,
  updated_at     INTEGER NOT NULL
);
CREATE TABLE remotes (
  id                   INTEGER PRIMARY KEY,
  repo_id              INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  target_id            INTEGER NOT NULL REFERENCES targets(id) ON DELETE RESTRICT,
  namespace            TEXT    NOT NULL DEFAULT '',
  name                 TEXT    NOT NULL,
  url                  TEXT    NOT NULL DEFAULT '',
  on_divergence        TEXT    NOT NULL DEFAULT 'alert',
  enabled              INTEGER NOT NULL DEFAULT 1,
  provisioned          INTEGER NOT NULL DEFAULT 0,
  atomic_unsupported   INTEGER NOT NULL DEFAULT 0,
  state                TEXT    NOT NULL DEFAULT 'pending',
  last_error           TEXT    NOT NULL DEFAULT '',
  consecutive_failures INTEGER NOT NULL DEFAULT 0,
  next_retry_at        INTEGER NOT NULL DEFAULT 0,
  last_sync_at         INTEGER NOT NULL DEFAULT 0,
  last_success_at      INTEGER NOT NULL DEFAULT 0,
  created_at           INTEGER NOT NULL,
  UNIQUE (repo_id, target_id, namespace, name)
);
CREATE INDEX remotes_retry ON remotes (next_retry_at) WHERE next_retry_at > 0;
CREATE TABLE ref_state (
  remote_id   INTEGER NOT NULL REFERENCES remotes(id) ON DELETE CASCADE,
  ref         TEXT    NOT NULL,
  sha         TEXT    NOT NULL,
  observed_at INTEGER NOT NULL,
  PRIMARY KEY (remote_id, ref)
);
CREATE TABLE pushed_state (
  remote_id INTEGER NOT NULL REFERENCES remotes(id) ON DELETE CASCADE,
  ref       TEXT    NOT NULL,
  sha       TEXT    NOT NULL,
  pushed_at INTEGER NOT NULL,
  PRIMARY KEY (remote_id, ref)
);
CREATE TABLE divergences (
  remote_id    INTEGER NOT NULL REFERENCES remotes(id) ON DELETE CASCADE,
  ref          TEXT    NOT NULL,
  hub_sha      TEXT    NOT NULL DEFAULT '',
  remote_sha   TEXT    NOT NULL DEFAULT '',
  expected_sha TEXT    NOT NULL DEFAULT '',
  reason       TEXT    NOT NULL DEFAULT '',
  detected_at  INTEGER NOT NULL,
  PRIMARY KEY (remote_id, ref)
);
CREATE TABLE sync_runs (
  id           INTEGER PRIMARY KEY,
  repo_id      INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  remote_id    INTEGER NOT NULL REFERENCES remotes(id) ON DELETE CASCADE,
  trigger      TEXT    NOT NULL,
  status       TEXT    NOT NULL,
  refs_changed INTEGER NOT NULL DEFAULT 0,
  error        TEXT    NOT NULL DEFAULT '',
  detail       TEXT    NOT NULL DEFAULT '',
  started_at   INTEGER NOT NULL,
  finished_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX sync_runs_repo ON sync_runs (repo_id, id);
CREATE TABLE tokens (
  id           INTEGER PRIMARY KEY,
  name         TEXT    NOT NULL UNIQUE,
  token_hash   TEXT    NOT NULL UNIQUE,
  scopes       TEXT    NOT NULL,
  repos        TEXT    NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE leases (
  name       TEXT    PRIMARY KEY,
  holder     TEXT    NOT NULL,
  expires_at INTEGER NOT NULL
);
`,
	// 2: outbound webhooks. The URL is encrypted because chat-bot URLs embed
	// their access key.
	`
CREATE TABLE webhooks (
  id               INTEGER PRIMARY KEY,
  name             TEXT    NOT NULL UNIQUE,
  url              BLOB    NOT NULL,
  url_host         TEXT    NOT NULL DEFAULT '',
  format           TEXT    NOT NULL DEFAULT 'json',
  events           TEXT    NOT NULL DEFAULT '*',
  repos            TEXT    NOT NULL DEFAULT '',
  secret           BLOB,
  enabled          INTEGER NOT NULL DEFAULT 1,
  last_status      TEXT    NOT NULL DEFAULT '',
  last_delivery_at INTEGER NOT NULL DEFAULT 0,
  created_at       INTEGER NOT NULL
);
`,
	// 3: adoption of pre-existing mirror repositories; binding name templates.
	`
ALTER TABLE remotes ADD COLUMN adopt INTEGER NOT NULL DEFAULT 0;
ALTER TABLE bindings ADD COLUMN name_template TEXT NOT NULL DEFAULT '{name}';
`,
	// 4: SSH public keys, authorized like tokens.
	`
CREATE TABLE ssh_keys (
  id           INTEGER PRIMARY KEY,
  name         TEXT    NOT NULL UNIQUE,
  fingerprint  TEXT    NOT NULL UNIQUE,
  public_key   TEXT    NOT NULL,
  scopes       TEXT    NOT NULL,
  repos        TEXT    NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER NOT NULL DEFAULT 0
);
`,
	// 5: per-target Git LFS object size limit.
	`
ALTER TABLE targets ADD COLUMN max_lfs_object_size INTEGER NOT NULL DEFAULT 0;
`,
	// 6: audit log of administrative changes and pushes.
	`
CREATE TABLE audit_log (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  at          INTEGER NOT NULL,
  actor       TEXT    NOT NULL,
  via         TEXT    NOT NULL,
  client_addr TEXT    NOT NULL DEFAULT '',
  action      TEXT    NOT NULL,
  path        TEXT    NOT NULL DEFAULT '',
  repo        TEXT    NOT NULL DEFAULT '',
  status      INTEGER NOT NULL DEFAULT 0,
  detail      TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX audit_log_repo ON audit_log (repo, id);
CREATE INDEX audit_log_at ON audit_log (at);
`,
	// 7: releases kept on the hub and published to mirrors.
	`
CREATE TABLE releases (
  id          INTEGER PRIMARY KEY,
  repo_id     INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  tag         TEXT    NOT NULL,
  name        TEXT    NOT NULL DEFAULT '',
  body        TEXT    NOT NULL DEFAULT '',
  prerelease  INTEGER NOT NULL DEFAULT 0,
  published   INTEGER NOT NULL DEFAULT 0,
  adopt       INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  UNIQUE (repo_id, tag)
);
CREATE TABLE release_assets (
  id           INTEGER PRIMARY KEY,
  release_id   INTEGER NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
  name         TEXT    NOT NULL,
  size         INTEGER NOT NULL,
  sha256       TEXT    NOT NULL,
  content_type TEXT    NOT NULL DEFAULT 'application/octet-stream',
  created_at   INTEGER NOT NULL,
  UNIQUE (release_id, name)
);
CREATE TABLE release_mirrors (
  release_id     INTEGER NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
  remote_id      INTEGER NOT NULL REFERENCES remotes(id) ON DELETE CASCADE,
  remote_release TEXT    NOT NULL DEFAULT '',
  state          TEXT    NOT NULL DEFAULT 'pending',
  synced_hash    TEXT    NOT NULL DEFAULT '',
  assets         TEXT    NOT NULL DEFAULT '{}',
  last_error     TEXT    NOT NULL DEFAULT '',
  failures       INTEGER NOT NULL DEFAULT 0,
  next_retry_at  INTEGER NOT NULL DEFAULT 0,
  updated_at     INTEGER NOT NULL,
  PRIMARY KEY (release_id, remote_id)
);
`,
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	var current int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this binary (%d)", current, len(migrations))
	}
	for i := current; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, i+1, Now()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func isForeignKey(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func wrapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotFound
	case isUnique(err):
		return ErrConflict
	default:
		return err
	}
}
