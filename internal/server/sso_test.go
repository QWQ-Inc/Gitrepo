package server_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/client"
	"github.com/qwq-inc/gitrepo/internal/config"
)

// fakeIdP is a minimal OpenID Connect provider (authorization code flow
// with PKCE, RS256 ID tokens, userinfo) that approves every request with
// the identity in claims.
type fakeIdP struct {
	t      *testing.T
	srv    *httptest.Server
	key    *rsa.PrivateKey
	client string
	secret string

	mu     sync.Mutex
	claims map[string]any       // the person who "signs in"
	codes  map[string]fakeGrant // code → grant
	// Tamper hooks.
	badNonce  bool
	badSig    bool
	noIDToken bool
	laxPKCE   bool // accept any code_verifier, like a provider without PKCE
	// What the last authorization request asked for.
	lastAuth url.Values
}

type fakeGrant struct {
	nonce, challenge, redirect string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{t: t, key: key, client: "gitrepo-client", secret: "s3cret", codes: map[string]fakeGrant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.srv.URL,
			"authorization_endpoint":                f.srv.URL + "/authorize",
			"token_endpoint":                        f.srv.URL + "/token",
			"userinfo_endpoint":                     f.srv.URL + "/userinfo",
			"jwks_uri":                              f.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
			"n": b64(f.key.N.Bytes()), "e": b64(big.NewInt(int64(f.key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.mu.Lock()
		f.lastAuth = q
		code := fmt.Sprintf("code-%d", len(f.codes)+1)
		f.codes[code] = fakeGrant{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri")}
		f.mu.Unlock()
		if q.Get("client_id") != f.client || q.Get("code_challenge_method") != "S256" || q.Get("response_type") != "code" {
			http.Error(w, "bad authorization request", http.StatusBadRequest)
			return
		}
		u, _ := url.Parse(q.Get("redirect_uri"))
		v := url.Values{"code": {code}, "state": {q.Get("state")}}
		u.RawQuery = v.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		id, sec, ok := r.BasicAuth()
		if !ok {
			id, sec = r.FormValue("client_id"), r.FormValue("client_secret")
		}
		if id != f.client || sec != f.secret {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		g, ok := f.codes[r.FormValue("code")]
		delete(f.codes, r.FormValue("code"))
		claims := map[string]any{}
		for k, v := range f.claims {
			claims[k] = v
		}
		badNonce, badSig, noID, lax := f.badNonce, f.badSig, f.noIDToken, f.laxPKCE
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(r.FormValue("code_verifier")))
		if !ok || (!lax && g.challenge != base64.RawURLEncoding.EncodeToString(sum[:])) || g.redirect != r.FormValue("redirect_uri") {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		claims["iss"], claims["aud"] = f.srv.URL, f.client
		claims["iat"], claims["exp"] = time.Now().Unix(), time.Now().Add(time.Hour).Unix()
		claims["nonce"] = g.nonce
		if badNonce {
			claims["nonce"] = "other"
		}
		out := map[string]any{"access_token": "at-" + r.FormValue("code"), "token_type": "Bearer", "expires_in": 3600}
		if !noID {
			out["id_token"] = f.sign(claims, badSig)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer at-") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(f.claims)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (f *fakeIdP) sign(claims map[string]any, tamper bool) string {
	h, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "k1"})
	p, _ := json.Marshal(claims)
	in := b64(h) + "." + b64(p)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		f.t.Fatal(err)
	}
	if tamper {
		sig[0] ^= 0xff
	}
	return in + "." + b64(sig)
}

func (f *fakeIdP) set(claims map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims = claims
}

// tamper changes the misbehaviour switches.
func (f *fakeIdP) tamper(badSig, badNonce, noIDToken bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.badSig, f.badNonce, f.noIDToken = badSig, badNonce, noIDToken
}

func (f *fakeIdP) lastAuthorize() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastAuth
}

// browser follows the sign-in redirects like a browser and returns the
// console fragment it lands on ("/sso/<code>" or "/login?error=...").
func browser(t *testing.T, start string) string {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if strings.HasPrefix(req.URL.Path, "/ui/") {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	resp, err := c.Get(start)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(loc, "/ui/#") {
		t.Fatalf("sign-in ended at %d %q", resp.StatusCode, loc)
	}
	return strings.TrimPrefix(loc, "/ui/#")
}

func TestSSOSignIn(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.SSO.SessionTTL = config.Duration(time.Hour) })
	idp := newFakeIdP(t)

	// Validation: who may sign in must be said; the issuer must be https
	// (loopback is fine for tests).
	for _, bad := range []map[string]any{
		{"name": "x", "issuer": idp.srv.URL, "client_id": "c"},
		{"name": "x", "issuer": "http://idp.example.com", "client_id": "c", "allow": []string{"*"}},
		{"name": "X Y", "issuer": idp.srv.URL, "client_id": "c", "allow": []string{"*"}},
		{"name": "x", "client_id": "c", "allow": []string{"*"}},
		{"name": "x", "issuer": idp.srv.URL, "client_id": "c", "allow": []string{"alice"}},
	} {
		if err := e.api.Do(t.Context(), http.MethodPost, "/api/sso/providers", bad, nil); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
	var p client.SSOProvider
	e.do(http.MethodPost, "/api/sso/providers", map[string]any{
		"name": "se", "kind": "stackenigma", "issuer": idp.srv.URL, "client_id": idp.client, "client_secret": idp.secret,
		"allow": []string{"*@example.com", "group:ops"},
	}, &p)
	if p.DisplayName != "StackEnigma" || !p.ClientSecret || p.RedirectURL != e.cfg.PublicURL+"/auth/oidc/se/callback" ||
		strings.Join(p.Scopes, " ") != "openid profile email" {
		t.Fatalf("provider: %+v", p)
	}
	var chk client.SSOCheck
	e.do(http.MethodPost, "/api/sso/providers/se/check", nil, &chk)
	if chk.Mode != "oidc" || chk.AuthURL != idp.srv.URL+"/authorize" {
		t.Fatalf("check: %+v", chk)
	}
	// The sign-in page lists it (no token needed).
	var methods []map[string]string
	if err := client.New(e.cfg.PublicURL, "").Do(t.Context(), http.MethodGet, "/api/sso/methods", nil, &methods); err != nil || len(methods) != 1 || methods[0]["kind"] != "stackenigma" {
		t.Fatalf("methods: %v %v", methods, err)
	}

	login := e.cfg.PublicURL + "/auth/oidc/se/login"
	exchange := func(frag string) (*client.Client, string) {
		t.Helper()
		code, ok := strings.CutPrefix(frag, "/sso/")
		if !ok {
			t.Fatalf("sign-in refused: %s", frag)
		}
		var res struct {
			Token     string    `json:"token"`
			Name      string    `json:"name"`
			ExpiresAt time.Time `json:"expires_at"`
		}
		anon := client.New(e.cfg.PublicURL, "")
		if err := anon.Do(t.Context(), http.MethodPost, "/api/sso/exchange", map[string]string{"code": code}, &res); err != nil {
			t.Fatal(err)
		}
		if time.Until(res.ExpiresAt) > time.Hour || time.Until(res.ExpiresAt) < 50*time.Minute {
			t.Fatalf("session expiry %v", res.ExpiresAt)
		}
		// The code is single use.
		if err := anon.Do(t.Context(), http.MethodPost, "/api/sso/exchange", map[string]string{"code": code}, nil); err == nil {
			t.Fatal("a sign-in code worked twice")
		}
		return client.New(e.cfg.PublicURL, res.Token), res.Name
	}

	// A verified address in the allowed domain signs in and is an admin.
	idp.set(map[string]any{"sub": "u1", "email": "alice@example.com", "email_verified": true})
	c, name := exchange(browser(t, login))
	if name != "sso:se:alice@example.com" {
		t.Fatalf("session name %q", name)
	}
	if q := idp.lastAuthorize(); q.Get("nonce") == "" || q.Get("state") == "" || !strings.Contains(q.Get("scope"), "openid") {
		t.Fatalf("authorization request: %v", q)
	}
	var repos []client.Repo
	if err := c.Do(t.Context(), http.MethodGet, "/api/repos", nil, &repos); err != nil {
		t.Fatalf("session cannot use the API: %v", err)
	}
	// Group membership is enough, too.
	idp.set(map[string]any{"sub": "u2", "email": "bob@other.org", "email_verified": true, "groups": []string{"dev", "ops"}})
	exchange(browser(t, login))

	// Refused: an unverified address, another domain, a forged signature,
	// a replayed nonce, a missing ID token.
	refused := func(why string) {
		t.Helper()
		frag := browser(t, login)
		if !strings.HasPrefix(frag, "/login?error=") {
			t.Fatalf("%s: signed in (%s)", why, frag)
		}
	}
	idp.set(map[string]any{"sub": "u3", "email": "mallory@example.com", "email_verified": false})
	refused("unverified e-mail")
	idp.set(map[string]any{"sub": "u4", "email": "eve@evil.com", "email_verified": true})
	refused("other domain")
	idp.set(map[string]any{"sub": "u1", "email": "alice@example.com", "email_verified": true})
	idp.tamper(true, false, false)
	refused("forged signature")
	idp.tamper(false, true, false)
	refused("wrong nonce")
	idp.tamper(false, false, true)
	refused("no ID token")
	idp.tamper(false, false, false)

	// A callback without the browser's state cookie is refused.
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).
		Get(e.cfg.PublicURL + "/auth/oidc/se/callback?code=code-1&state=x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Location"), "/ui/#/login?error=") {
		t.Fatalf("stateless callback: %s", resp.Header.Get("Location"))
	}

	// Sessions are listed; signing out ends one; changing who may sign in
	// ends the rest.
	var sessions []client.Session
	e.do(http.MethodGet, "/api/sso/sessions", nil, &sessions)
	if len(sessions) != 2 {
		t.Fatalf("sessions: %+v", sessions)
	}
	if err := c.Do(t.Context(), http.MethodPost, "/api/sso/logout", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Do(t.Context(), http.MethodGet, "/api/repos", nil, nil); err == nil {
		t.Fatal("the session works after signing out")
	}
	c2, _ := exchange(browser(t, login))
	e.do(http.MethodPatch, "/api/sso/providers/se", map[string]any{"allow": []string{"carol@example.com"}}, nil)
	if err := c2.Do(t.Context(), http.MethodGet, "/api/repos", nil, nil); err == nil {
		t.Fatal("a session outlived the rule that allowed it")
	}
	refused("no longer allowed")

	// Sign-ins are in the audit log.
	var audit struct {
		Entries []struct {
			Actor  string `json:"actor"`
			Action string `json:"action"`
			Detail string `json:"detail"`
		} `json:"entries"`
	}
	e.do(http.MethodGet, "/api/audit?limit=100", nil, &audit)
	var ok, no int
	for _, a := range audit.Entries {
		if a.Action == "sign in" {
			if strings.HasPrefix(a.Detail, "refused") {
				no++
			} else {
				ok++
			}
		}
	}
	if ok < 3 || no < 6 {
		t.Fatalf("audit: %d sign-ins, %d refusals: %+v", ok, no, audit.Entries)
	}

	// Disabling hides the provider.
	e.do(http.MethodPatch, "/api/sso/providers/se", map[string]any{"enabled": false}, nil)
	refused("disabled")
}

// A plain OAuth 2.0 server: no discovery, no ID token; the identity comes
// from the userinfo endpoint.
func TestSSOPlainOAuth2(t *testing.T) {
	e := newEnv(t)
	idp := newFakeIdP(t)
	idp.tamper(false, false, true)
	e.do(http.MethodPost, "/api/sso/providers", map[string]any{
		"name": "forge", "auth_url": idp.srv.URL + "/authorize", "token_url": idp.srv.URL + "/token",
		"userinfo_url": idp.srv.URL + "/userinfo", "client_id": idp.client, "client_secret": idp.secret,
		"allow": []string{"sub:42"},
	}, nil)
	idp.set(map[string]any{"id": 42, "login": "alice", "email": "alice@example.com"})
	frag := browser(t, e.cfg.PublicURL+"/auth/oidc/forge/login")
	if !strings.HasPrefix(frag, "/sso/") {
		t.Fatalf("plain OAuth 2.0 sign-in refused: %s", frag)
	}
	if idp.lastAuthorize().Get("nonce") != "" {
		t.Fatal("a nonce was sent to a plain OAuth 2.0 server")
	}
	// Login CSRF: an attacker's own (valid) code planted in a victim's
	// browser that has a sign-in in progress is refused on the state, with a
	// provider that does not enforce PKCE (and, in plain OAuth 2.0, no nonce
	// either: the state is the only defence left).
	idp.mu.Lock()
	idp.laxPKCE = true
	idp.mu.Unlock()
	noFollow := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := (&http.Client{CheckRedirect: noFollow}).Get(idp.srv.URL + "/authorize?" + url.Values{
		"client_id": {idp.client}, "redirect_uri": {e.cfg.PublicURL + "/auth/oidc/forge/callback"}, "response_type": {"code"},
		"code_challenge": {"attacker"}, "code_challenge_method": {"S256"}, "state": {"evil"},
	}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	planted, _ := url.Parse(resp.Header.Get("Location"))
	jar, _ := cookiejar.New(nil)
	victim := &http.Client{Jar: jar, CheckRedirect: noFollow}
	if resp, err = victim.Get(e.cfg.PublicURL + "/auth/oidc/forge/login"); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp, err = victim.Get(planted.String()); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Location"), "/ui/#/login?error=") {
		t.Fatalf("a planted code signed the victim in: %s", resp.Header.Get("Location"))
	}
	idp.mu.Lock()
	idp.laxPKCE = false
	idp.mu.Unlock()
	idp.set(map[string]any{"id": 43, "login": "bob"})
	if frag := browser(t, e.cfg.PublicURL+"/auth/oidc/forge/login"); !strings.HasPrefix(frag, "/login?error=") {
		t.Fatalf("subject 43 signed in: %s", frag)
	}
}
