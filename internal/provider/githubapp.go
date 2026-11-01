package provider

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/qwq-inc/gitrepo/internal/store"
)

// A GitHub App credential is the App's ID and private key. Gitrepo signs a
// short JWT with the key and exchanges it for an installation access token
// (valid for an hour, limited to the App's permissions and the repositories
// it is installed on), which then works like a personal token for the API
// and for git. Nothing long-lived belongs to a person.

// CredGitHubApp is the credential kind of GitHub Apps.
const CredGitHubApp = "github-app"

// tokenMargin is how long a cached installation token (valid for an hour)
// must stay valid to be reused: longer than the longest operation that uses
// one - a release publish (30 minutes) or a sync (push_timeout, 10 minutes
// by default).
const tokenMargin = 40 * time.Minute

type cachedToken struct {
	mu      sync.Mutex
	token   string
	expires time.Time
}

var appTokens struct {
	sync.Mutex
	m map[string]*cachedToken
}

// ResolveSecret turns a target's stored secret into the one used for API
// calls and git: unchanged for tokens and passwords, a fresh (cached)
// installation access token for GitHub Apps.
func ResolveSecret(ctx context.Context, t *store.Target, stored string) (string, error) {
	if t.CredKind != CredGitHubApp {
		return stored, nil
	}
	return (&GitHub{}).installationToken(ctx, t, stored)
}

// ParseAppKey parses a GitHub App private key (PKCS#1 or PKCS#8 PEM).
func ParseAppKey(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(pemText)))
	if block == nil {
		return nil, errors.New("the GitHub App private key is not a PEM file")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("the GitHub App private key is not an RSA key (download it from the App's settings page)")
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the GitHub App private key is not an RSA key")
	}
	return rk, nil
}

// appJWT authenticates as the App itself (10 minutes at most; issued a
// minute in the past to tolerate clock drift).
func appJWT(appID string, key *rsa.PrivateKey, now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": appID})
	if err != nil {
		return "", err
	}
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

func (g *GitHub) appHeaders(t *store.Target, keyPEM string) (map[string]string, error) {
	if t.AppID == "" {
		return nil, errors.New("github-app credential needs the App ID")
	}
	key, err := ParseAppKey(keyPEM)
	if err != nil {
		return nil, err
	}
	jwt, err := appJWT(t.AppID, key, time.Now())
	if err != nil {
		return nil, err
	}
	return g.headers(jwt), nil
}

func (g *GitHub) installationToken(ctx context.Context, t *store.Target, keyPEM string) (string, error) {
	if t.InstallationID == "" {
		return "", errors.New("github-app credential has no installation ID; update the target so it is looked up again")
	}
	sum := sha256.Sum256([]byte(keyPEM))
	cacheKey := g.api(t) + "|" + t.AppID + "|" + t.InstallationID + "|" + hex.EncodeToString(sum[:])
	appTokens.Lock()
	if appTokens.m == nil {
		appTokens.m = map[string]*cachedToken{}
	}
	c := appTokens.m[cacheKey]
	if c == nil {
		c = &cachedToken{}
		appTokens.m[cacheKey] = c
	}
	appTokens.Unlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.expires) > tokenMargin {
		return c.token, nil
	}
	h, err := g.appHeaders(t, keyPEM)
	if err != nil {
		return "", err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	u := g.api(t) + "/app/installations/" + url.PathEscape(t.InstallationID) + "/access_tokens"
	if _, err := doJSON(ctx, http.MethodPost, u, h, nil, &out); err != nil {
		return "", fmt.Errorf("github app %s: get an installation token: %w", t.AppID, err)
	}
	if out.Token == "" {
		return "", fmt.Errorf("github app %s: no installation token in the response", t.AppID)
	}
	if out.ExpiresAt.IsZero() {
		out.ExpiresAt = time.Now().Add(time.Hour)
	}
	c.token, c.expires = out.Token, out.ExpiresAt
	return c.token, nil
}

// FindAppInstallation returns the ID of the App's installation on the
// target's namespace (an organization or a user account).
func FindAppInstallation(ctx context.Context, t *store.Target, keyPEM string) (string, error) {
	g := &GitHub{}
	h, err := g.appHeaders(t, keyPEM)
	if err != nil {
		return "", err
	}
	if t.Namespace == "" {
		return "", errors.New("set --namespace (the organization or user the App is installed on) or pass the installation ID")
	}
	var out struct {
		ID json.Number `json:"id"`
	}
	ns := url.PathEscape(t.Namespace)
	_, err = doJSON(ctx, http.MethodGet, g.api(t)+"/orgs/"+ns+"/installation", h, nil, &out)
	if statusOf(err) == http.StatusNotFound {
		_, err = doJSON(ctx, http.MethodGet, g.api(t)+"/users/"+ns+"/installation", h, nil, &out)
	}
	if statusOf(err) == http.StatusNotFound {
		return "", fmt.Errorf("github app %s is not installed on %s; install it there (Settings → GitHub Apps) first", t.AppID, t.Namespace)
	}
	if err != nil {
		return "", fmt.Errorf("github app %s: %w", t.AppID, err)
	}
	return idString(out.ID), nil
}

// checkApp verifies an installation token and describes its reach.
func (g *GitHub) checkApp(ctx context.Context, t *store.Target, token string) (string, error) {
	var out struct {
		TotalCount int `json:"total_count"`
	}
	if _, err := doJSON(ctx, http.MethodGet, g.api(t)+"/installation/repositories?per_page=1", g.headers(token), nil, &out); err != nil {
		return "", err
	}
	return fmt.Sprintf("GitHub App %s, installation %s (%d repositories)", t.AppID, t.InstallationID, out.TotalCount), nil
}
