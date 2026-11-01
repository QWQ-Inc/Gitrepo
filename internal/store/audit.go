package store

import (
	"context"
	"strings"
	"unicode/utf8"
)

// AuditEntry records one administrative change or push.
type AuditEntry struct {
	ID int64 `json:"id"`
	At int64 `json:"-"`
	// Actor is the token, SSH key or admin principal that acted.
	Actor string `json:"actor"`
	// Via is "api", "http" or "ssh".
	Via        string `json:"via"`
	ClientAddr string `json:"client_addr,omitempty"`
	// Action is the API route ("DELETE /api/targets/{name}") or "git push".
	Action string `json:"action"`
	Path   string `json:"path,omitempty"`
	Repo   string `json:"repo,omitempty"`
	// Status is the HTTP status of an API call; 0 for pushes.
	Status int    `json:"status,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// AuditFilter selects audit entries. Zero fields match everything.
type AuditFilter struct {
	Repo  string
	Actor string
	// Action matches as a substring of the action.
	Action string
	// Before returns entries with a smaller ID (pagination).
	Before int64
	Limit  int
}

const maxAuditDetail = 4096

// AddAudit appends an entry.
func (s *Store) AddAudit(ctx context.Context, e *AuditEntry) error {
	if e.At == 0 {
		e.At = Now()
	}
	if len(e.Detail) > maxAuditDetail {
		cut := maxAuditDetail - 3
		for cut > 0 && !utf8.RuneStart(e.Detail[cut]) {
			cut--
		}
		e.Detail = e.Detail[:cut] + "..."
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO audit_log (at, actor, via, client_addr, action, path, repo, status, detail)
VALUES (?,?,?,?,?,?,?,?,?)`, e.At, e.Actor, e.Via, e.ClientAddr, e.Action, e.Path, e.Repo, e.Status, e.Detail)
	if err != nil {
		return err
	}
	e.ID, _ = res.LastInsertId()
	return nil
}

// ListAudit returns matching entries, newest first.
func (s *Store) ListAudit(ctx context.Context, f AuditFilter) ([]*AuditEntry, error) {
	q := `SELECT id, at, actor, via, client_addr, action, path, repo, status, detail FROM audit_log WHERE 1=1`
	var args []any
	if f.Repo != "" {
		q += ` AND repo = ?`
		args = append(args, f.Repo)
	}
	if f.Actor != "" {
		q += ` AND actor = ?`
		args = append(args, f.Actor)
	}
	if f.Action != "" {
		q += ` AND action LIKE ? ESCAPE '\'`
		args = append(args, "%"+likeEscaper.Replace(f.Action)+"%")
	}
	if f.Before > 0 {
		q += ` AND id < ?`
		args = append(args, f.Before)
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Via, &e.ClientAddr, &e.Action, &e.Path, &e.Repo, &e.Status, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// PruneAudit deletes entries recorded before the cutoff (unix ms).
func (s *Store) PruneAudit(ctx context.Context, before int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM audit_log WHERE at < ?`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
