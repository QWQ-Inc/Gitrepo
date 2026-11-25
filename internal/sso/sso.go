// Package sso signs people in to the console through an identity provider:
// an OpenID Connect issuer (StackEnigma, Keycloak, Authentik, Google, Azure
// AD, GitLab, ...) or a plain OAuth 2.0 server with a userinfo endpoint.
//
// The flow is the authorization code flow with PKCE, state and nonce. The
// browser state lives in a short-lived cookie sealed with the instance key,
// so nothing is stored for logins that are never finished. A successful
// sign-in creates a session (an expiring token, see auth.SessionPrefix) and
// hands it to the console through a one-time code in the URL fragment: the
// session token itself never appears in a URL.
package sso

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/qwq-inc/gitrepo/internal/auth"
	"github.com/qwq-inc/gitrepo/internal/secrets"
	"github.com/qwq-inc/gitrepo/internal/store"
)

// Kinds. A kind is a preset: it only changes the defaults and how the
// sign-in button looks.
const (
	KindOIDC        = "oidc"
	KindStackEnigma = "stackenigma"
)

// Kinds lists the valid kinds.
var Kinds = []string{KindOIDC, KindStackEnigma}

// DefaultScopes are requested from an OpenID Connect issuer.
var DefaultScopes = []string{oidc.ScopeOpenID, "profile", "email"}

const (
	cookieName = "gitrepo_sso"
	stateTTL   = 10 * time.Minute
	codeTTL    = 2 * time.Minute
	discoTTL   = time.Hour
)

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Claims is what Gitrepo uses from an identity.
type Claims struct {
	Subject       string   `json:"sub"`
	Email         string   `json:"email,omitempty"`
	EmailVerified bool     `json:"email_verified"`
	Name          string   `json:"name,omitempty"`
	Username      string   `json:"preferred_username,omitempty"`
	Groups        []string `json:"groups,omitempty"`
}

// Ident is how the person is shown: e-mail, user name or subject.
func (c Claims) Ident() string {
	switch {
	case c.Email != "":
		return c.Email
	case c.Username != "":
		return c.Username
	}
	return c.Subject
}

// Event describes a sign-in attempt, for the audit log.
type Event struct {
	Provider   string
	Ident      string
	ClientAddr string
	OK         bool
	Detail     string
}

// Manager runs sign-ins.
type Manager struct {
	Store     *store.Store
	Box       *secrets.Box
	PublicURL string
	Log       *slog.Logger
	// SessionTTL is how long a sign-in lasts.
	SessionTTL time.Duration
	// Client is used for discovery, token and userinfo requests.
	Client *http.Client
	// OnSignIn is told about every finished attempt (may be nil).
	OnSignIn func(Event)
	// ClientAddr returns the caller's address (may be nil).
	ClientAddr func(*http.Request) string

	mu    sync.Mutex
	disco map[string]*discovered
	codes map[string]*pending
}

type discovered struct {
	key      string // name|issuer|updated_at
	at       time.Time
	provider *oidc.Provider
}

type pending struct {
	token   string
	name    string
	expires time.Time // session expiry
	until   time.Time // code expiry
}

// Result is a finished sign-in, as handed to the console.
type Result struct {
	Token     string    `json:"token"`
	Name      string    `json:"name"`
	ExpiresAt time.Time `json:"expires_at"`
}

// RedirectURL is the callback address to register with the provider.
func (m *Manager) RedirectURL(name string) string {
	return strings.TrimRight(m.PublicURL, "/") + "/auth/oidc/" + name + "/callback"
}

// ------------------------------------------------------------------ config

// Normalize fills defaults and checks a provider's settings.
func Normalize(p *store.OIDCProvider) error {
	p.Name = strings.TrimSpace(p.Name)
	if !nameRE.MatchString(p.Name) {
		return fmt.Errorf("invalid name %q: use lowercase letters, digits and dashes (it appears in the callback URL)", p.Name)
	}
	if p.Name == store.LocalProvider {
		return fmt.Errorf("%q is reserved for password sign-ins", p.Name)
	}
	if p.Kind == "" {
		p.Kind = KindOIDC
	}
	if !slices.Contains(Kinds, p.Kind) {
		return fmt.Errorf("unknown kind %q (valid: %s)", p.Kind, strings.Join(Kinds, ", "))
	}
	if p.DisplayName == "" {
		p.DisplayName = p.Name
		if p.Kind == KindStackEnigma {
			p.DisplayName = "StackEnigma"
		}
	}
	p.Issuer = strings.TrimRight(strings.TrimSpace(p.Issuer), "/")
	p.AuthURL, p.TokenURL, p.UserinfoURL = strings.TrimSpace(p.AuthURL), strings.TrimSpace(p.TokenURL), strings.TrimSpace(p.UserinfoURL)
	if strings.TrimSpace(p.ClientID) == "" {
		return errors.New("client_id is required")
	}
	if p.Issuer != "" {
		if err := checkURL("issuer", p.Issuer); err != nil {
			return err
		}
		if len(p.Scopes) == 0 {
			p.Scopes = append([]string(nil), DefaultScopes...)
		}
		if !slices.Contains(p.Scopes, oidc.ScopeOpenID) {
			p.Scopes = append([]string{oidc.ScopeOpenID}, p.Scopes...)
		}
	} else {
		if p.AuthURL == "" || p.TokenURL == "" || p.UserinfoURL == "" {
			return errors.New("give the issuer (OpenID Connect), or all of auth_url, token_url and userinfo_url (plain OAuth 2.0)")
		}
	}
	for field, u := range map[string]string{"auth_url": p.AuthURL, "token_url": p.TokenURL, "userinfo_url": p.UserinfoURL} {
		if u != "" {
			if err := checkURL(field, u); err != nil {
				return err
			}
		}
	}
	if p.GroupsClaim == "" {
		p.GroupsClaim = "groups"
	}
	var allow []string
	for _, a := range p.Allow {
		if a = strings.TrimSpace(a); a == "" {
			continue
		}
		if err := checkRule(a); err != nil {
			return err
		}
		allow = append(allow, a)
	}
	if len(allow) == 0 {
		return errors.New(`say who may sign in (allow): e-mail addresses or patterns like "*@example.com", "group:NAME", "sub:SUBJECT", or "*" for everyone the provider accepts`)
	}
	p.Allow = allow
	return nil
}

func checkURL(field, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s: invalid URL %q", field, raw)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopback(u.Hostname()):
	default:
		return fmt.Errorf("%s must be an https URL", field)
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func checkRule(a string) error {
	switch {
	case a == "*":
	case strings.HasPrefix(a, "group:"):
		if strings.TrimPrefix(a, "group:") == "" {
			return fmt.Errorf("allow %q: missing group name", a)
		}
	case strings.HasPrefix(a, "sub:"):
		if strings.TrimPrefix(a, "sub:") == "" {
			return fmt.Errorf("allow %q: missing subject", a)
		}
	default:
		if !strings.Contains(a, "@") {
			return fmt.Errorf(`allow %q: expected an e-mail address or pattern ("*@example.com"), "group:NAME", "sub:SUBJECT" or "*"`, a)
		}
		if _, err := path.Match(a, "x@y"); err != nil {
			return fmt.Errorf("allow %q: %v", a, err)
		}
	}
	return nil
}

// Allowed reports whether the rules let c sign in. E-mail rules only match
// an address the provider says is verified.
func Allowed(rules []string, c Claims) bool {
	email := strings.ToLower(c.Email)
	for _, a := range rules {
		switch {
		case a == "*":
			return true
		case strings.HasPrefix(a, "group:"):
			if slices.Contains(c.Groups, strings.TrimPrefix(a, "group:")) {
				return true
			}
		case strings.HasPrefix(a, "sub:"):
			if c.Subject != "" && c.Subject == strings.TrimPrefix(a, "sub:") {
				return true
			}
		default:
			if email != "" && c.EmailVerified {
				if ok, _ := path.Match(strings.ToLower(a), email); ok {
					return true
				}
			}
		}
	}
	return false
}

// ---------------------------------------------------------------- endpoints

type endpoints struct {
	oauth    oauth2.Endpoint
	verifier *oidc.IDTokenVerifier // nil for plain OAuth 2.0
	userinfo string
}

func (m *Manager) client() *http.Client {
	if m.Client != nil {
		return m.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (m *Manager) ctx(ctx context.Context) context.Context {
	c := m.client()
	return oidc.ClientContext(context.WithValue(ctx, oauth2.HTTPClient, c), c)
}

func (m *Manager) endpoints(ctx context.Context, p *store.OIDCProvider) (*endpoints, error) {
	if p.Issuer == "" {
		return &endpoints{
			oauth:    oauth2.Endpoint{AuthURL: p.AuthURL, TokenURL: p.TokenURL},
			userinfo: p.UserinfoURL,
		}, nil
	}
	key := fmt.Sprintf("%s|%s|%d", p.Name, p.Issuer, p.UpdatedAt)
	m.mu.Lock()
	d := m.disco[p.Name]
	m.mu.Unlock()
	if d == nil || d.key != key || time.Since(d.at) > discoTTL {
		prov, err := oidc.NewProvider(m.ctx(ctx), p.Issuer)
		if err != nil {
			return nil, fmt.Errorf("read the provider's OpenID configuration: %w", err)
		}
		d = &discovered{key: key, at: time.Now(), provider: prov}
		m.mu.Lock()
		if m.disco == nil {
			m.disco = map[string]*discovered{}
		}
		m.disco[p.Name] = d
		m.mu.Unlock()
	}
	ep := d.provider.Endpoint()
	if p.AuthURL != "" {
		ep.AuthURL = p.AuthURL
	}
	if p.TokenURL != "" {
		ep.TokenURL = p.TokenURL
	}
	ui := p.UserinfoURL
	if ui == "" {
		ui = d.provider.UserInfoEndpoint()
	}
	return &endpoints{
		oauth:    ep,
		verifier: d.provider.Verifier(&oidc.Config{ClientID: p.ClientID}),
		userinfo: ui,
	}, nil
}

func (m *Manager) config(p *store.OIDCProvider, ep *endpoints) (*oauth2.Config, error) {
	var secret string
	if len(p.ClientSecret) > 0 {
		b, err := m.Box.Open(p.ClientSecret, p.AAD())
		if err != nil {
			return nil, fmt.Errorf("decrypt the client secret: %w", err)
		}
		secret = string(b)
	}
	return &oauth2.Config{
		ClientID: p.ClientID, ClientSecret: secret,
		Endpoint: ep.oauth, RedirectURL: m.RedirectURL(p.Name), Scopes: p.Scopes,
	}, nil
}

// CheckResult describes a provider's endpoints, for the admin.
type CheckResult struct {
	Mode        string `json:"mode"` // "oidc" | "oauth2"
	AuthURL     string `json:"auth_url"`
	TokenURL    string `json:"token_url"`
	UserinfoURL string `json:"userinfo_url,omitempty"`
	RedirectURL string `json:"redirect_url"`
}

// Check resolves a provider's endpoints (reading the issuer's discovery
// document) and reports them.
func (m *Manager) Check(ctx context.Context, p *store.OIDCProvider) (*CheckResult, error) {
	ep, err := m.endpoints(ctx, p)
	if err != nil {
		return nil, err
	}
	if _, err := m.config(p, ep); err != nil {
		return nil, err
	}
	mode := "oidc"
	if ep.verifier == nil {
		mode = "oauth2"
	}
	return &CheckResult{Mode: mode, AuthURL: ep.oauth.AuthURL, TokenURL: ep.oauth.TokenURL, UserinfoURL: ep.userinfo, RedirectURL: m.RedirectURL(p.Name)}, nil
}

// --------------------------------------------------------------------- flow

type state struct {
	Provider string `json:"p"`
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Expires  int64  `json:"e"`
}

func random() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (m *Manager) secure() bool { return strings.HasPrefix(m.PublicURL, "https://") }

// Login starts a sign-in: GET /auth/oidc/{name}/login.
func (m *Manager) Login(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p, err := m.Store.GetOIDCProvider(r.Context(), name)
	if err != nil || !p.Enabled {
		m.fail(w, r, name, "", "this sign-in method is not available", err)
		return
	}
	ep, err := m.endpoints(r.Context(), p)
	if err != nil {
		m.fail(w, r, name, "", "the identity provider cannot be reached", err)
		return
	}
	conf, err := m.config(p, ep)
	if err != nil {
		m.fail(w, r, name, "", "the sign-in method is misconfigured", err)
		return
	}
	st := state{Provider: name, State: random(), Nonce: random(), Verifier: oauth2.GenerateVerifier(), Expires: time.Now().Add(stateTTL).Unix()}
	raw, _ := json.Marshal(st)
	sealed, err := m.Box.Seal(raw, "sso-state")
	if err != nil {
		m.fail(w, r, name, "", "internal error", err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: base64.RawURLEncoding.EncodeToString(sealed), Path: "/auth/oidc/",
		MaxAge: int(stateTTL / time.Second), HttpOnly: true, Secure: m.secure(), SameSite: http.SameSiteLaxMode,
	})
	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(st.Verifier)}
	if ep.verifier != nil {
		opts = append(opts, oidc.Nonce(st.Nonce))
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, conf.AuthCodeURL(st.State, opts...), http.StatusFound)
}

// Callback finishes a sign-in: GET /auth/oidc/{name}/callback.
func (m *Manager) Callback(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	st, err := m.readState(r)
	// The state cookie is single use.
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/auth/oidc/", MaxAge: -1, HttpOnly: true, Secure: m.secure(), SameSite: http.SameSiteLaxMode})
	q := r.URL.Query()
	switch {
	case err != nil:
		m.fail(w, r, name, "", "the sign-in expired or was started in another browser; try again", err)
		return
	case st.Provider != name || subtle.ConstantTimeCompare([]byte(st.State), []byte(q.Get("state"))) != 1:
		m.fail(w, r, name, "", "the sign-in does not match the one this browser started; try again", errors.New("state mismatch"))
		return
	case q.Get("error") != "":
		msg := q.Get("error")
		if d := q.Get("error_description"); d != "" {
			msg += ": " + d
		}
		m.fail(w, r, name, "", "the identity provider refused the sign-in ("+clip(msg, 200)+")", errors.New(msg))
		return
	case q.Get("code") == "":
		m.fail(w, r, name, "", "the identity provider returned no authorization code", errors.New("no code"))
		return
	}
	p, err := m.Store.GetOIDCProvider(r.Context(), name)
	if err != nil || !p.Enabled {
		m.fail(w, r, name, "", "this sign-in method is not available", err)
		return
	}
	c, err := m.identify(r.Context(), p, q.Get("code"), st)
	if err != nil {
		m.fail(w, r, name, "", "the sign-in could not be verified", err)
		return
	}
	if !Allowed(p.Allow, c) {
		m.fail(w, r, name, c.Ident(), c.Ident()+" is not allowed to sign in to this Gitrepo", errors.New("not allowed"))
		return
	}
	res, err := m.newSession(r.Context(), p, c)
	if err != nil {
		m.fail(w, r, name, c.Ident(), "internal error", err)
		return
	}
	code := random()
	m.mu.Lock()
	if m.codes == nil {
		m.codes = map[string]*pending{}
	}
	now := time.Now()
	for k, v := range m.codes {
		if now.After(v.until) {
			delete(m.codes, k)
		}
	}
	m.codes[code] = &pending{token: res.Token, name: res.Name, expires: res.ExpiresAt, until: now.Add(codeTTL)}
	m.mu.Unlock()
	m.event(r, name, c.Ident(), true, "")
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/ui/#/sso/"+code, http.StatusFound)
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func (m *Manager) readState(r *http.Request) (*state, error) {
	ck, err := r.Cookie(cookieName)
	if err != nil {
		return nil, errors.New("no sign-in state cookie")
	}
	blob, err := base64.RawURLEncoding.DecodeString(ck.Value)
	if err != nil {
		return nil, err
	}
	raw, err := m.Box.Open(blob, "sso-state")
	if err != nil {
		return nil, err
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	if time.Now().Unix() > st.Expires {
		return nil, errors.New("sign-in state expired")
	}
	return &st, nil
}

// identify exchanges the code and returns the verified identity.
func (m *Manager) identify(ctx context.Context, p *store.OIDCProvider, code string, st *state) (Claims, error) {
	ep, err := m.endpoints(ctx, p)
	if err != nil {
		return Claims{}, err
	}
	conf, err := m.config(p, ep)
	if err != nil {
		return Claims{}, err
	}
	octx := m.ctx(ctx)
	tok, err := conf.Exchange(octx, code, oauth2.VerifierOption(st.Verifier))
	if err != nil {
		return Claims{}, fmt.Errorf("exchange the authorization code: %w", err)
	}
	raw := map[string]any{}
	if ep.verifier != nil {
		idRaw, _ := tok.Extra("id_token").(string)
		if idRaw == "" {
			return Claims{}, errors.New("the provider returned no ID token")
		}
		idt, err := ep.verifier.Verify(octx, idRaw)
		if err != nil {
			return Claims{}, fmt.Errorf("verify the ID token: %w", err)
		}
		if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(st.Nonce)) != 1 {
			return Claims{}, errors.New("the ID token's nonce does not match")
		}
		if err := idt.Claims(&raw); err != nil {
			return Claims{}, err
		}
	}
	// Plain OAuth 2.0 has only the userinfo endpoint; an ID token may lack
	// the e-mail address or groups, which userinfo then supplies.
	_, hasEmail := raw["email"]
	_, hasGroups := raw[p.GroupsClaim]
	if ep.userinfo != "" && (ep.verifier == nil || !hasEmail || !hasGroups) {
		info, err := m.userinfo(octx, ep.userinfo, tok)
		if err != nil {
			if ep.verifier == nil {
				return Claims{}, err
			}
			m.Log.Warn("sso: userinfo", "provider", p.Name, "err", err)
		} else {
			if sub, _ := raw["sub"].(string); sub != "" && claimString(info, "sub") != sub {
				return Claims{}, errors.New("the userinfo subject does not match the ID token")
			}
			for k, v := range info {
				if _, ok := raw[k]; !ok {
					raw[k] = v
				}
			}
		}
	}
	return parseClaims(raw, p.GroupsClaim)
}

func (m *Manager) userinfo(ctx context.Context, endpoint string, tok *oauth2.Token) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	tok.SetAuthHeader(req)
	resp, err := m.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("userinfo: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo: %s", resp.Status)
	}
	out := map[string]any{}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("userinfo: %w (a signed userinfo response is not supported)", err)
	}
	return out, nil
}

func claimString(m map[string]any, k string) string {
	switch v := m[k].(type) {
	case string:
		return v
	case float64:
		// Some OAuth 2.0 servers (GitHub, Gitea) use numeric user IDs.
		return fmt.Sprintf("%.0f", v)
	case json.Number:
		return v.String()
	}
	return ""
}

func parseClaims(raw map[string]any, groupsClaim string) (Claims, error) {
	c := Claims{
		Subject:  claimString(raw, "sub"),
		Email:    claimString(raw, "email"),
		Name:     claimString(raw, "name"),
		Username: claimString(raw, "preferred_username"),
	}
	if c.Subject == "" {
		c.Subject = claimString(raw, "id") // plain OAuth 2.0 userinfo
	}
	if c.Username == "" {
		c.Username = claimString(raw, "login")
	}
	if c.Subject == "" {
		return Claims{}, errors.New("the identity has no subject")
	}
	// email_verified is a boolean, or a string with some providers. When it
	// is missing, the provider is trusted to give verified addresses only.
	switch v := raw["email_verified"].(type) {
	case bool:
		c.EmailVerified = v
	case string:
		c.EmailVerified = v == "true"
	case nil:
		c.EmailVerified = c.Email != ""
	}
	switch v := raw[groupsClaim].(type) {
	case []any:
		for _, g := range v {
			if s, ok := g.(string); ok && s != "" {
				c.Groups = append(c.Groups, s)
			}
		}
	case string:
		c.Groups = strings.Fields(v)
	}
	sort.Strings(c.Groups)
	return c, nil
}

func (m *Manager) newSession(ctx context.Context, p *store.OIDCProvider, c Claims) (*Result, error) {
	plain, hash := auth.NewSessionToken()
	ttl := m.SessionTTL
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	exp := time.Now().Add(ttl)
	x := &store.Session{
		Hash: hash, Provider: p.Name, Subject: c.Subject, Email: c.Email,
		Name:   "sso:" + p.Name + ":" + c.Ident(),
		Scopes: []string{auth.ScopeAdmin}, ExpiresAt: exp.UnixMilli(),
	}
	if err := m.Store.CreateSession(ctx, x); err != nil {
		return nil, err
	}
	return &Result{Token: plain, Name: x.Name, ExpiresAt: exp.UTC()}, nil
}

// Exchange turns the one-time code from the callback into the session.
func (m *Manager) Exchange(code string) (*Result, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.codes[code]
	if p == nil {
		return nil, false
	}
	delete(m.codes, code)
	if time.Now().After(p.until) {
		return nil, false
	}
	return &Result{Token: p.token, Name: p.name, ExpiresAt: p.expires.UTC()}, true
}

// fail sends the browser back to the console's sign-in page with a message.
func (m *Manager) fail(w http.ResponseWriter, r *http.Request, provider, ident, msg string, err error) {
	if err != nil {
		m.Log.Warn("sso: sign-in failed", "provider", provider, "ident", ident, "err", err)
	}
	m.event(r, provider, ident, false, msg)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/ui/#/login?error="+url.QueryEscape(msg), http.StatusFound)
}

func (m *Manager) event(r *http.Request, provider, ident string, ok bool, detail string) {
	if m.OnSignIn == nil {
		return
	}
	addr := ""
	if m.ClientAddr != nil {
		addr = m.ClientAddr(r)
	}
	m.OnSignIn(Event{Provider: provider, Ident: ident, ClientAddr: addr, OK: ok, Detail: detail})
}
