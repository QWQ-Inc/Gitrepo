package store

import (
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
