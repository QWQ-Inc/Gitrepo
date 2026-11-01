package store

import (
	"context"
	"strings"
)

// ReplaceTokens makes the token table exactly tokens (an edge node
// mirroring its hub), keyed by hash: unchanged tokens keep their rows.
func (s *Store) ReplaceTokens(ctx context.Context, tokens []Token) error {
	return s.replaceKeyed(ctx, "tokens", "token_hash", len(tokens), func(i int) (string, []any) {
		t := tokens[i]
		return t.Hash, []any{t.Name, strings.Join(t.Scopes, ","), strings.Join(t.Repos, ",")}
	}, `UPDATE tokens SET name=?, scopes=?, repos=? WHERE token_hash=?`,
		`INSERT INTO tokens (name, scopes, repos, token_hash, created_at) VALUES (?,?,?,?,?)`)
}

// ReplaceSSHKeys makes the SSH key table exactly keys, keyed by
// fingerprint: unchanged keys keep their rows (and open connections).
func (s *Store) ReplaceSSHKeys(ctx context.Context, keys []SSHKey) error {
	return s.replaceKeyed(ctx, "ssh_keys", "fingerprint", len(keys), func(i int) (string, []any) {
		k := keys[i]
		return k.Fingerprint, []any{k.Name, k.PublicKey, strings.Join(k.Scopes, ","), strings.Join(k.Repos, ",")}
	}, `UPDATE ssh_keys SET name=?, public_key=?, scopes=?, repos=? WHERE fingerprint=?`,
		`INSERT INTO ssh_keys (name, public_key, scopes, repos, fingerprint, created_at) VALUES (?,?,?,?,?,?)`)
}

func (s *Store) replaceKeyed(ctx context.Context, table, keyCol string, n int, row func(int) (string, []any), update, insert string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	keep := map[string]bool{}
	for i := 0; i < n; i++ {
		k, _ := row(i)
		keep[k] = true
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+keyCol+` FROM `+table)
	if err != nil {
		return err
	}
	var existing []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return err
		}
		existing = append(existing, k)
	}
	rows.Close()
	have := map[string]bool{}
	for _, k := range existing {
		if keep[k] {
			have[k] = true
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+keyCol+`=?`, k); err != nil {
			return err
		}
	}
	// Free names first: a name may move from one key to another.
	for i := 0; i < n; i++ {
		k, vals := row(i)
		if have[k] {
			if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET name=name||' (renaming)' WHERE `+keyCol+`=? AND name<>?`, k, vals[0]); err != nil {
				return err
			}
		}
	}
	for i := 0; i < n; i++ {
		k, vals := row(i)
		var err error
		if have[k] {
			_, err = tx.ExecContext(ctx, update, append(vals, k)...)
		} else {
			_, err = tx.ExecContext(ctx, insert, append(vals, k, Now())...)
		}
		if err != nil {
			return wrapErr(err)
		}
	}
	return tx.Commit()
}
