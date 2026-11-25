package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qwq-inc/gitrepo/internal/store"
)

// ErrInvalidToken is returned for credentials that match no token.
var ErrInvalidToken = errors.New("invalid token")

// Authenticator resolves request credentials to principals.
type Authenticator struct {
	Store *store.Store
	// AdminHash is Hash(bootstrap admin token); empty disables it.
	AdminHash string
	// Limiter slows down password guessing (nil: no limit).
	Limiter *Limiter
	// ClientAddr returns the caller's address for the limiter (nil: the
	// connection's address).
	ClientAddr func(*http.Request) string

	mu       sync.Mutex
	verified map[string]time.Time // recent password successes
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
// Basic auth is a token (any user name), the bootstrap admin token, or an
// account's user name and password.
func (a *Authenticator) FromRequest(r *http.Request) (*Principal, error) {
	if user, pass, ok := r.BasicAuth(); ok && user != "" && pass != "" && !looksLikeToken(pass) && !a.IsAdminToken(pass) {
		u, err := a.Password(r.Context(), user, pass, a.addr(r))
		if err != nil {
			return nil, err
		}
		return UserPrincipal(u), nil
	}
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
	if strings.HasPrefix(tok, SessionPrefix) {
		x, err := a.Store.GetSessionByHash(ctx, h)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, ErrInvalidToken
			}
			return nil, err
		}
		_ = a.Store.TouchSession(ctx, x.ID)
		p := &Principal{Name: x.Name, Scopes: x.Scopes, SessionID: x.ID}
		if x.Provider == store.LocalProvider {
			p.UserID, _ = strconv.ParseInt(x.Subject, 10, 64)
			if u, err := a.Store.GetUserByID(ctx, p.UserID); err == nil {
				p.Repos = u.Repos
			}
		}
		return p, nil
	}
	t, err := a.Store.GetTokenByHash(ctx, h)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrInvalidToken
		}
		return nil, err
	}
	if t.Hash == h {
		_ = a.Store.TouchToken(ctx, t.ID)
	} else {
		_ = a.Store.TouchTokenPrev(ctx, t.ID) // the value before a rotation
	}
	return &Principal{Name: t.Name, Scopes: t.Scopes, Repos: t.Repos}, nil
}

func (a *Authenticator) addr(r *http.Request) string {
	if a.ClientAddr != nil {
		return a.ClientAddr(r)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
