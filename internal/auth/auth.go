// Package auth implements client tokens: generation, hashing and
// scope/repository authorization checks.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"slices"
	"strings"
)

// Scopes. Each scope implies the ones listed after it:
// admin ⇒ create ⇒ write ⇒ read.
const (
	ScopeRead   = "read"
	ScopeWrite  = "write"
	ScopeCreate = "create"
	ScopeAdmin  = "admin"
)

var scopeRank = map[string]int{ScopeRead: 1, ScopeWrite: 2, ScopeCreate: 3, ScopeAdmin: 4}

// ValidScope reports whether s is a known scope.
func ValidScope(s string) bool { _, ok := scopeRank[s]; return ok }

// TokenPrefix marks Gitrepo tokens so they are easy to spot in leaks.
const TokenPrefix = "grp_"

// NewToken returns a new random token and its storage hash.
func NewToken() (plain, hash string) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	plain = TokenPrefix + hex.EncodeToString(b)
	return plain, Hash(plain)
}

// Hash returns the hex SHA-256 of a token. Tokens are high-entropy random
// strings, so a fast hash is appropriate.
func Hash(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// Principal is an authenticated caller.
type Principal struct {
	Name   string
	Scopes []string
	// Repos are slug patterns ("org/name", "org/*", "*"). Empty means all.
	Repos []string
}

// Admin returns the bootstrap administrator principal.
func Admin() *Principal {
	return &Principal{Name: "admin", Scopes: []string{ScopeAdmin}}
}

func (p *Principal) maxRank() int {
	m := 0
	for _, s := range p.Scopes {
		if r := scopeRank[s]; r > m {
			m = r
		}
	}
	return m
}

// IsAdmin reports whether p has the admin scope.
func (p *Principal) IsAdmin() bool { return p != nil && p.maxRank() >= scopeRank[ScopeAdmin] }

// Can reports whether p holds scope for the repository slug.
func (p *Principal) Can(scope, slug string) bool {
	if p == nil {
		return false
	}
	if p.maxRank() < scopeRank[scope] {
		return false
	}
	if p.IsAdmin() {
		return true
	}
	return MatchRepo(p.Repos, slug)
}

// MatchRepo reports whether slug matches any pattern. An empty pattern list
// or "*" matches everything; otherwise path.Match semantics apply per
// segment ("org/*").
func MatchRepo(patterns []string, slug string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if p == "*" || p == "*/*" {
			return true
		}
		if ok, _ := path.Match(p, slug); ok {
			return true
		}
	}
	return false
}

// ParseScopes splits and validates a comma-separated scope list.
func ParseScopes(s string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !ValidScope(part) {
			return nil, fmt.Errorf("unknown scope %q (valid: read, write, create, admin)", part)
		}
		if !slices.Contains(out, part) {
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one scope is required")
	}
	return out, nil
}

// ParseRepoPatterns splits and validates a comma-separated pattern list.
func ParseRepoPatterns(s string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, err := path.Match(part, "x/y"); err != nil {
			return nil, fmt.Errorf("invalid repo pattern %q", part)
		}
		out = append(out, part)
	}
	return out, nil
}
