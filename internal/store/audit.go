package store

import (
	"context"
	"fmt"
	"strings"
	"unicode"
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

// AddAudit appends an entry. Fields are made safe to print first: some
// come from the request (the path of a refused call can hold anything), and
// the log is read in terminals.
func (s *Store) AddAudit(ctx context.Context, e *AuditEntry) error {
	if e.At == 0 {
		e.At = Now()
	}
	e.Actor = cleanText(e.Actor, 128)
	e.Via = cleanText(e.Via, 16)
	e.ClientAddr = cleanText(e.ClientAddr, 64)
	e.Action = cleanText(e.Action, 256)
	e.Path = cleanText(e.Path, 512)
	e.Repo = cleanText(e.Repo, 256)
	e.Detail = cleanText(e.Detail, maxAuditDetail)
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

// cleanText escapes control and formatting characters (newlines, terminal
// escapes, bidi overrides) and invalid UTF-8, and caps the length in bytes
// at a character boundary.
func cleanText(s string, max int) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
		i += size
	}
	out := b.String()
	if len(out) > max {
		cut := max - 3
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = out[:cut] + "..."
	}
	return out
}
