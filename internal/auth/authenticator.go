package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/qwq-inc/gitrepo/internal/store"
)

// ErrInvalidToken is returned for credentials that match no token.
var ErrInvalidToken = errors.New("invalid token")

// Authenticator resolves request credentials to principals.
type Authenticator struct {
	Store *store.Store
	// AdminHash is Hash(bootstrap admin token); empty disables it.
	AdminHash string
}

// TokenFromRequest extracts a token from Basic auth (password, or username
// when the password is empty) or a Bearer/token Authorization header.
func TokenFromRequest(r *http.Request) string {
	if user, pass, ok := r.BasicAuth(); ok {
		if pass != "" {
			return pass
		}
		return user
	}
	h := r.Header.Get("Authorization")
	for _, prefix := range []string{"Bearer ", "bearer ", "token ", "Token "} {
		if v, ok := strings.CutPrefix(h, prefix); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// FromRequest returns the principal for r. It returns (nil, nil) when the
// request carries no credentials and ErrInvalidToken when they are wrong.
func (a *Authenticator) FromRequest(r *http.Request) (*Principal, error) {
	tok := TokenFromRequest(r)
	if tok == "" {
		return nil, nil
	}
	return a.FromToken(r.Context(), tok)
}

// FromToken resolves a plaintext token.
func (a *Authenticator) FromToken(ctx context.Context, tok string) (*Principal, error) {
	h := Hash(tok)
	if a.AdminHash != "" && subtle.ConstantTimeCompare([]byte(h), []byte(a.AdminHash)) == 1 {
		return Admin(), nil
	}
	t, err := a.Store.GetTokenByHash(ctx, h)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrInvalidToken
		}
		return nil, err
	}
	_ = a.Store.TouchToken(ctx, t.ID)
	return &Principal{Name: t.Name, Scopes: t.Scopes, Repos: t.Repos}, nil
}
