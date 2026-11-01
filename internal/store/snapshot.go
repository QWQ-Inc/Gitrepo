package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
)

// SnapshotTo writes a consistent copy of the database at src to dest, which
// must not exist. It uses VACUUM INTO, so it is safe while a server is
// using the database (WAL mode lets it read without blocking writers).
func SnapshotTo(src, dest string) error {
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("database: %w", err)
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(30000)")
	db, err := sql.Open("sqlite", "file:"+src+"?"+q.Encode())
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`VACUUM INTO ?`, dest); err != nil {
		return fmt.Errorf("snapshot database: %w", err)
	}
	return os.Chmod(dest, 0o600)
}

// ClearLeases drops all leases, e.g. after restoring a database copied from
// another (running) instance.
func (s *Store) ClearLeases(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM leases`)
	return err
}

// MarkAllRebuilt changes every repository's purge time, so edge nodes
// discard their copies and fetch them again: after restoring an older
// snapshot, objects pushed since it was taken must not stay fetchable (by
// SHA) from an edge although the hub no longer has them.
func (s *Store) MarkAllRebuilt(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE repos SET purged_at = MAX(purged_at + 1, ?)`, Now())
	return err
}
