package store

import (
	"context"
	"strings"
)

// OIDCProvider is an identity provider people sign in to the console with:
// an OpenID Connect issuer (endpoints from its discovery document), or a
// plain OAuth 2.0 server whose endpoints are given by hand.
type OIDCProvider struct {
	ID          int64
	Name        string // used in URLs: /auth/oidc/{name}/...
	DisplayName string
	Kind        string // "oidc" or a preset ("stackenigma"); only changes the button
	Issuer      string
	AuthURL     string
	TokenURL    string
	UserinfoURL string
	ClientID    string
	// ClientSecret is sealed with the provider's AAD (may be empty for
	// public clients, which then rely on PKCE alone).
	ClientSecret []byte
	Scopes       []string
	GroupsClaim  string
	// Allow lists who may sign in: verified e-mail addresses or patterns
	// ("*@example.com"), "group:NAME", "sub:SUBJECT", or "*".
	Allow     []string
	Enabled   bool
	CreatedAt int64
	UpdatedAt int64
}

// AAD binds the provider's client secret to it.
func (p *OIDCProvider) AAD() string { return "oidc:" + p.Name + ":client_secret" }

const oidcCols = `id, name, display_name, kind, issuer, auth_url, token_url, userinfo_url, client_id, client_secret,
scopes, groups_claim, allow, enabled, created_at, updated_at`

func scanOIDC(sc interface{ Scan(...any) error }) (*OIDCProvider, error) {
	var p OIDCProvider
	var scopes, allow string
	var enabled int
	if err := sc.Scan(&p.ID, &p.Name, &p.DisplayName, &p.Kind, &p.Issuer, &p.AuthURL, &p.TokenURL, &p.UserinfoURL,
		&p.ClientID, &p.ClientSecret, &scopes, &p.GroupsClaim, &allow, &enabled, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, wrapErr(err)
	}
	p.Scopes, p.Allow, p.Enabled = strings.Fields(scopes), splitLines(allow), enabled != 0
	return &p, nil
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (s *Store) CreateOIDCProvider(ctx context.Context, p *OIDCProvider) error {
	p.CreatedAt = Now()
	p.UpdatedAt = p.CreatedAt
	res, err := s.db.ExecContext(ctx, `INSERT INTO oidc_providers (name, display_name, kind, issuer, auth_url, token_url, userinfo_url,
client_id, client_secret, scopes, groups_claim, allow, enabled, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.Name, p.DisplayName, p.Kind, p.Issuer, p.AuthURL, p.TokenURL, p.UserinfoURL, p.ClientID, p.ClientSecret,
		strings.Join(p.Scopes, " "), p.GroupsClaim, strings.Join(p.Allow, "\n"), b2i(p.Enabled), p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return wrapErr(err)
	}
	p.ID, _ = res.LastInsertId()
	return nil
}

// UpdateOIDCProvider saves every field of p (by name).
func (s *Store) UpdateOIDCProvider(ctx context.Context, p *OIDCProvider) error {
	p.UpdatedAt = Now()
	res, err := s.db.ExecContext(ctx, `UPDATE oidc_providers SET display_name=?, kind=?, issuer=?, auth_url=?, token_url=?, userinfo_url=?,
client_id=?, client_secret=?, scopes=?, groups_claim=?, allow=?, enabled=?, updated_at=? WHERE name=?`,
		p.DisplayName, p.Kind, p.Issuer, p.AuthURL, p.TokenURL, p.UserinfoURL, p.ClientID, p.ClientSecret,
		strings.Join(p.Scopes, " "), p.GroupsClaim, strings.Join(p.Allow, "\n"), b2i(p.Enabled), p.UpdatedAt, p.Name)
	if err != nil {
		return wrapErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetOIDCProvider(ctx context.Context, name string) (*OIDCProvider, error) {
	return scanOIDC(s.db.QueryRowContext(ctx, `SELECT `+oidcCols+` FROM oidc_providers WHERE name=?`, name))
}

func (s *Store) ListOIDCProviders(ctx context.Context) ([]*OIDCProvider, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+oidcCols+` FROM oidc_providers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*OIDCProvider
	for rows.Next() {
		p, err := scanOIDC(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeleteOIDCProvider removes a provider and signs out everyone who signed
// in through it.
func (s *Store) DeleteOIDCProvider(ctx context.Context, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM oidc_providers WHERE name=?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE provider=?`, name); err != nil {
		return err
	}
	return tx.Commit()
}

// ------------------------------------------------------------------ sessions

// Session is a console sign-in through an identity provider. Its token
// works like an API token until it expires or is revoked.
type Session struct {
	ID         int64
	Hash       string
	Provider   string
	Subject    string
	Name       string // shown in the audit log: "sso:<provider>:<email or subject>"
	Email      string
	Scopes     []string
	CreatedAt  int64
	ExpiresAt  int64
	LastUsedAt int64
}

const sessionCols = `id, token_hash, provider, subject, name, email, scopes, created_at, expires_at, last_used_at`

func scanSession(sc interface{ Scan(...any) error }) (*Session, error) {
	var x Session
	var scopes string
	if err := sc.Scan(&x.ID, &x.Hash, &x.Provider, &x.Subject, &x.Name, &x.Email, &scopes, &x.CreatedAt, &x.ExpiresAt, &x.LastUsedAt); err != nil {
		return nil, wrapErr(err)
	}
	x.Scopes = splitList(scopes)
	return &x, nil
}

func (s *Store) CreateSession(ctx context.Context, x *Session) error {
	x.CreatedAt = Now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO sessions (token_hash, provider, subject, name, email, scopes, created_at, expires_at)
VALUES (?,?,?,?,?,?,?,?)`, x.Hash, x.Provider, x.Subject, x.Name, x.Email, strings.Join(x.Scopes, ","), x.CreatedAt, x.ExpiresAt)
	if err != nil {
		return wrapErr(err)
	}
	x.ID, _ = res.LastInsertId()
	return nil
}

// GetSessionByHash returns an unexpired session.
func (s *Store) GetSessionByHash(ctx context.Context, hash string) (*Session, error) {
	return scanSession(s.db.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE token_hash=? AND expires_at>?`, hash, Now()))
}

// ListSessions returns the unexpired sessions, newest first.
func (s *Store) ListSessions(ctx context.Context) ([]*Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE expires_at>? ORDER BY created_at DESC`, Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Session
	for rows.Next() {
		x, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) DeleteSession(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteSessionByHash signs out the session with this token (no error when
// there is none).
func (s *Store) DeleteSessionByHash(ctx context.Context, hash string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, hash)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteProviderSessions signs out everyone who signed in through provider.
func (s *Store) DeleteProviderSessions(ctx context.Context, provider string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE provider=?`, provider)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneSessions deletes expired sessions.
func (s *Store) PruneSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at<=?`, Now())
	return err
}

func (s *Store) TouchSession(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_used_at=? WHERE id=?`, Now(), id)
	return err
}
