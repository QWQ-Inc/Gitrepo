package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"github.com/qwq-inc/gitrepo/internal/store"
)

// ErrLocked is returned while too many wrong passwords were tried. It is
// also an ErrInvalidToken, so callers that only know that one refuse it.
var ErrLocked = fmt.Errorf("%w: too many failed sign-ins; try again later", ErrInvalidToken)

var usernameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,38}$`)

// NormalizeUsername lowercases a user name and checks it.
func NormalizeUsername(s string) (string, error) {
	u := strings.ToLower(strings.TrimSpace(s))
	if !usernameRE.MatchString(u) {
		return "", errors.New("user names are 1-39 lowercase letters, digits, '.', '_' or '-'")
	}
	if u == Admin().Name {
		return "", fmt.Errorf("%q is reserved for the bootstrap admin token", u)
	}
	return u, nil
}

// CheckPassword enforces the password rules.
func CheckPassword(pw string) error {
	switch {
	case utf8.RuneCountInString(pw) < 8:
		return errors.New("the password must have at least 8 characters")
	case len(pw) > 72:
		return errors.New("the password must have at most 72 bytes")
	case strings.HasPrefix(pw, TokenPrefix) || strings.HasPrefix(pw, SessionPrefix):
		return errors.New("that is a token, not a password")
	}
	return nil
}

// HashPassword returns a bcrypt hash.
func HashPassword(pw string) (string, error) {
	if err := CheckPassword(pw); err != nil {
		return "", err
	}
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

// looksLikeToken reports whether a secret is a token rather than an
// account password (tokens are recognisable by their prefix).
func looksLikeToken(s string) bool {
	return strings.HasPrefix(s, TokenPrefix) || strings.HasPrefix(s, SessionPrefix) || strings.HasPrefix(s, "lfs1.")
}

// UserPrincipal is the principal of an account.
func UserPrincipal(u *store.User) *Principal {
	return &Principal{Name: "user:" + u.Username, Scopes: u.Scopes, Repos: u.Repos, UserID: u.ID}
}

// Password checks a user name and password and returns the account. A
// wrong user name and a wrong password look the same; repeated failures
// lock the user name (and, after more, the client address) for a while.
func (a *Authenticator) Password(ctx context.Context, username, password, client string) (*store.User, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	keys := []string{"user:" + username, "addr:" + client}
	if a.Limiter.Locked(keys...) {
		return nil, ErrLocked
	}
	u, err := a.Store.GetUser(ctx, username)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if u == nil {
		// Take as long as a real check: the timing must not tell which
		// user names exist.
		bcrypt.CompareHashAndPassword(dummyHash(), []byte(password))
	}
	if u == nil || u.Disabled || !a.verify(u, password) {
		a.Limiter.Fail(keys...)
		return nil, ErrInvalidToken
	}
	a.Limiter.Reset("user:" + username)
	return u, nil
}

// verify checks a password, remembering recent successes for a few
// minutes: git sends the credentials with every request of a clone or
// push, and bcrypt is slow on purpose. A changed password has a new hash,
// so it never matches an old entry.
func (a *Authenticator) verify(u *store.User, password string) bool {
	sum := sha256.Sum256([]byte(u.PasswordHash + "\x00" + password))
	key := hex.EncodeToString(sum[:])
	a.mu.Lock()
	until, ok := a.verified[key]
	a.mu.Unlock()
	if ok && time.Now().Before(until) {
		return true
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return false
	}
	a.mu.Lock()
	if a.verified == nil {
		a.verified = map[string]time.Time{}
	}
	now := time.Now()
	for k, t := range a.verified {
		if now.After(t) {
			delete(a.verified, k)
		}
	}
	a.verified[key] = now.Add(5 * time.Minute)
	a.mu.Unlock()
	return true
}

var dummyHash = sync.OnceValue(func() []byte {
	b, _ := bcrypt.GenerateFromPassword([]byte("not a password of anyone"), bcrypt.DefaultCost)
	return b
})

// IsAdminToken reports whether s is the bootstrap admin token.
func (a *Authenticator) IsAdminToken(s string) bool {
	return a.AdminHash != "" && s != "" && subtle.ConstantTimeCompare([]byte(Hash(s)), []byte(a.AdminHash)) == 1
}

// ------------------------------------------------------------------ limiter

// Limiter slows down password guessing: after a few failures for a key
// (a user name, a client address) further attempts are refused for a
// while, doubling up to 15 minutes. It lives in memory: a restart resets it.
type Limiter struct {
	// Free is the number of failures allowed per key before locking
	// (default 5); addresses get four times as many.
	Free int

	mu   sync.Mutex
	keys map[string]*attempts
}

type attempts struct {
	fails int
	until time.Time
	last  time.Time
}

func (l *Limiter) free(key string) int {
	n := l.Free
	if n <= 0 {
		n = 5
	}
	if strings.HasPrefix(key, "addr:") {
		n *= 4
	}
	return n
}

// Locked reports whether any key is locked.
func (l *Limiter) Locked(keys ...string) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	for _, k := range keys {
		if a := l.keys[k]; a != nil && now.Before(a.until) {
			return true
		}
	}
	return false
}

// Fail records a failure for each key.
func (l *Limiter) Fail(keys ...string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.keys == nil {
		l.keys = map[string]*attempts{}
	}
	now := time.Now()
	for k, a := range l.keys {
		if now.Sub(a.last) > time.Hour && now.After(a.until) {
			delete(l.keys, k)
		}
	}
	for _, k := range keys {
		a := l.keys[k]
		if a == nil {
			a = &attempts{}
			l.keys[k] = a
		}
		a.fails++
		a.last = now
		if over := a.fails - l.free(k); over >= 0 {
			d := time.Minute << min(over, 4)
			a.until = now.Add(min(d, 15*time.Minute))
		}
	}
}

// Reset forgets the failures of keys.
func (l *Limiter) Reset(keys ...string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		delete(l.keys, k)
	}
}
