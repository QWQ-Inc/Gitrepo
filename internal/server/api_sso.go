package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/qwq-inc/gitrepo/internal/auth"
	"github.com/qwq-inc/gitrepo/internal/sso"
	"github.com/qwq-inc/gitrepo/internal/store"
)

// registerSSO adds sign-in through identity providers: the browser flow,
// the console's public endpoints, and the admin API.
func (s *Server) registerSSO(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/oidc/{name}/login", s.sso.Login)
	mux.HandleFunc("GET /auth/oidc/{name}/callback", s.sso.Callback)
	mux.HandleFunc("GET /api/sso/methods", s.apiSSOMethods)
	mux.HandleFunc("POST /api/sso/exchange", s.apiSSOExchange)
	mux.HandleFunc("POST /api/sso/logout", s.apiSSOLogout)

	h := func(pattern string, fn http.HandlerFunc) { mux.HandleFunc(pattern, s.requireAdmin(fn)) }
	h("GET /api/sso/providers", s.apiListSSOProviders)
	h("POST /api/sso/providers", s.apiCreateSSOProvider)
	h("GET /api/sso/providers/{name}", s.apiGetSSOProvider)
	h("PATCH /api/sso/providers/{name}", s.apiUpdateSSOProvider)
	h("DELETE /api/sso/providers/{name}", s.apiDeleteSSOProvider)
	h("POST /api/sso/providers/{name}/check", s.apiCheckSSOProvider)
	h("GET /api/sso/sessions", s.apiListSessions)
	h("DELETE /api/sso/sessions/{id}", s.apiDeleteSession)
}

// ------------------------------------------------------------ public side

type ssoMethod struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Kind        string `json:"kind"`
	LoginURL    string `json:"login_url"`
}

// apiSSOMethods lists the sign-in buttons for the console's login page.
func (s *Server) apiSSOMethods(w http.ResponseWriter, r *http.Request) {
	providers, err := s.store.ListOIDCProviders(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []ssoMethod{}
	for _, p := range providers {
		if p.Enabled {
			out = append(out, ssoMethod{Name: p.Name, DisplayName: p.DisplayName, Kind: p.Kind, LoginURL: "/auth/oidc/" + p.Name + "/login"})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiSSOExchange(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code string `json:"code"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	res, ok := s.sso.Exchange(in.Code)
	if !ok {
		s.fail(w, &apiError{status: http.StatusUnauthorized, err: errors.New("the sign-in code is invalid or expired; sign in again")})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, res)
}

// apiSSOLogout ends the caller's session (a no-op for API tokens).
func (s *Server) apiSSOLogout(w http.ResponseWriter, r *http.Request) {
	tok := auth.TokenFromRequest(r)
	if strings.HasPrefix(tok, auth.SessionPrefix) {
		if _, err := s.store.DeleteSessionByHash(r.Context(), auth.Hash(tok)); err != nil {
			s.fail(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) auditSignIn(e sso.Event) {
	detail := "signed in"
	status := http.StatusOK
	if !e.OK {
		detail, status = "refused: "+e.Detail, http.StatusForbidden
	}
	actor := "sso:" + e.Provider
	if e.Ident != "" {
		actor += ":" + e.Ident
	}
	s.recordAudit(&store.AuditEntry{
		Actor: actor, Via: "sso", ClientAddr: e.ClientAddr, Action: "sign in",
		Status: status, Detail: detail,
	})
}

// -------------------------------------------------------------- admin side

type ssoProviderOut struct {
	Name         string    `json:"name"`
	DisplayName  string    `json:"display_name"`
	Kind         string    `json:"kind"`
	Issuer       string    `json:"issuer,omitempty"`
	AuthURL      string    `json:"auth_url,omitempty"`
	TokenURL     string    `json:"token_url,omitempty"`
	UserinfoURL  string    `json:"userinfo_url,omitempty"`
	ClientID     string    `json:"client_id"`
	ClientSecret bool      `json:"client_secret"`
	Scopes       []string  `json:"scopes"`
	GroupsClaim  string    `json:"groups_claim"`
	Allow        []string  `json:"allow"`
	Enabled      bool      `json:"enabled"`
	RedirectURL  string    `json:"redirect_url"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (s *Server) toSSOOut(p *store.OIDCProvider) ssoProviderOut {
	return ssoProviderOut{
		Name: p.Name, DisplayName: p.DisplayName, Kind: p.Kind, Issuer: p.Issuer, AuthURL: p.AuthURL,
		TokenURL: p.TokenURL, UserinfoURL: p.UserinfoURL, ClientID: p.ClientID, ClientSecret: len(p.ClientSecret) > 0,
		Scopes: p.Scopes, GroupsClaim: p.GroupsClaim, Allow: p.Allow, Enabled: p.Enabled,
		RedirectURL: s.sso.RedirectURL(p.Name), UpdatedAt: store.ToTime(p.UpdatedAt),
	}
}

func (s *Server) apiListSSOProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := s.store.ListOIDCProviders(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []ssoProviderOut{}
	for _, p := range providers {
		out = append(out, s.toSSOOut(p))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) ssoFromPath(r *http.Request) (*store.OIDCProvider, error) {
	p, err := s.store.GetOIDCProvider(r.Context(), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		return nil, &apiError{status: http.StatusNotFound, err: errors.New("sign-in provider not found")}
	}
	return p, err
}

func (s *Server) apiGetSSOProvider(w http.ResponseWriter, r *http.Request) {
	p, err := s.ssoFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toSSOOut(p))
}

// ssoIn is the body of create and update; on update, absent fields keep
// their value and an empty client_secret keeps the stored one (use
// clear_client_secret to remove it).
type ssoIn struct {
	Name              string    `json:"name"`
	DisplayName       *string   `json:"display_name"`
	Kind              *string   `json:"kind"`
	Issuer            *string   `json:"issuer"`
	AuthURL           *string   `json:"auth_url"`
	TokenURL          *string   `json:"token_url"`
	UserinfoURL       *string   `json:"userinfo_url"`
	ClientID          *string   `json:"client_id"`
	ClientSecret      *string   `json:"client_secret"`
	ClearClientSecret bool      `json:"clear_client_secret"`
	Scopes            *[]string `json:"scopes"`
	GroupsClaim       *string   `json:"groups_claim"`
	Allow             *[]string `json:"allow"`
	Enabled           *bool     `json:"enabled"`
}

func (in *ssoIn) apply(p *store.OIDCProvider) (newSecret *string) {
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = strings.TrimSpace(*v)
		}
	}
	set(&p.DisplayName, in.DisplayName)
	set(&p.Kind, in.Kind)
	set(&p.Issuer, in.Issuer)
	set(&p.AuthURL, in.AuthURL)
	set(&p.TokenURL, in.TokenURL)
	set(&p.UserinfoURL, in.UserinfoURL)
	set(&p.ClientID, in.ClientID)
	set(&p.GroupsClaim, in.GroupsClaim)
	if in.Scopes != nil {
		p.Scopes = nil
		for _, sc := range *in.Scopes {
			p.Scopes = append(p.Scopes, strings.Fields(sc)...)
		}
	}
	if in.Allow != nil {
		p.Allow = nil
		for _, a := range *in.Allow {
			for _, l := range strings.Split(a, "\n") {
				if l = strings.TrimSpace(l); l != "" {
					p.Allow = append(p.Allow, l)
				}
			}
		}
	}
	if in.Enabled != nil {
		p.Enabled = *in.Enabled
	}
	if in.ClientSecret != nil && *in.ClientSecret != "" {
		return in.ClientSecret
	}
	return nil
}

func (s *Server) apiCreateSSOProvider(w http.ResponseWriter, r *http.Request) {
	var in ssoIn
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	p := &store.OIDCProvider{Name: in.Name, Enabled: true}
	secret := in.apply(p)
	if err := sso.Normalize(p); err != nil {
		s.fail(w, badRequest("%v", err))
		return
	}
	if secret != nil {
		sealed, err := s.box.Seal([]byte(*secret), p.AAD())
		if err != nil {
			s.fail(w, err)
			return
		}
		p.ClientSecret = sealed
	}
	if err := s.store.CreateOIDCProvider(r.Context(), p); err != nil {
		if errors.Is(err, store.ErrConflict) {
			err = &apiError{status: http.StatusConflict, err: errors.New("a sign-in provider with this name exists")}
		}
		s.fail(w, err)
		return
	}
	noteAudit(r, "sign-in provider %s (%s), allow %s", p.Name, ssoWhere(p), strings.Join(p.Allow, ", "))
	writeJSON(w, http.StatusCreated, s.toSSOOut(p))
}

func ssoWhere(p *store.OIDCProvider) string {
	if p.Issuer != "" {
		return "issuer " + p.Issuer
	}
	return "OAuth 2.0 " + p.AuthURL
}

func (s *Server) apiUpdateSSOProvider(w http.ResponseWriter, r *http.Request) {
	p, err := s.ssoFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	var in ssoIn
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	if in.Name != "" && in.Name != p.Name {
		s.fail(w, badRequest("a provider cannot be renamed (its name is in the callback URL); add a new one"))
		return
	}
	before := *p
	secret := in.apply(p)
	if err := sso.Normalize(p); err != nil {
		s.fail(w, badRequest("%v", err))
		return
	}
	switch {
	case secret != nil:
		if p.ClientSecret, err = s.box.Seal([]byte(*secret), p.AAD()); err != nil {
			s.fail(w, err)
			return
		}
	case in.ClearClientSecret:
		p.ClientSecret = nil
	}
	if err := s.store.UpdateOIDCProvider(r.Context(), p); err != nil {
		s.fail(w, err)
		return
	}
	// Who may sign in, or through what, changed: sign everyone out of this
	// provider rather than let a session outlive the rule that allowed it.
	var revoked int64
	if !p.Enabled || before.Issuer != p.Issuer || before.AuthURL != p.AuthURL || before.TokenURL != p.TokenURL ||
		before.UserinfoURL != p.UserinfoURL || before.ClientID != p.ClientID || before.GroupsClaim != p.GroupsClaim ||
		strings.Join(before.Allow, "\n") != strings.Join(p.Allow, "\n") {
		if revoked, err = s.store.DeleteProviderSessions(r.Context(), p.Name); err != nil {
			s.fail(w, err)
			return
		}
	}
	noteAudit(r, "sign-in provider %s (%s), allow %s, enabled %t; %d session(s) ended", p.Name, ssoWhere(p), strings.Join(p.Allow, ", "), p.Enabled, revoked)
	writeJSON(w, http.StatusOK, s.toSSOOut(p))
}

func (s *Server) apiDeleteSSOProvider(w http.ResponseWriter, r *http.Request) {
	p, err := s.ssoFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.DeleteOIDCProvider(r.Context(), p.Name); err != nil {
		s.fail(w, err)
		return
	}
	noteAudit(r, "sign-in provider %s (%s)", p.Name, ssoWhere(p))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiCheckSSOProvider(w http.ResponseWriter, r *http.Request) {
	p, err := s.ssoFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	res, err := s.sso.Check(r.Context(), p)
	if err != nil {
		s.fail(w, &apiError{status: http.StatusBadGateway, err: err})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type sessionOut struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Provider   string     `json:"provider"`
	Email      string     `json:"email,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	Current    bool       `json:"current"`
}

func (s *Server) apiListSessions(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListSessions(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	cur := auth.Hash(auth.TokenFromRequest(r))
	out := []sessionOut{}
	for _, x := range list {
		out = append(out, sessionOut{ID: x.ID, Name: x.Name, Provider: x.Provider, Email: x.Email,
			CreatedAt: store.ToTime(x.CreatedAt), ExpiresAt: store.ToTime(x.ExpiresAt), LastUsedAt: ts(x.LastUsedAt), Current: x.Hash == cur})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiDeleteSession(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.fail(w, badRequest("invalid id"))
		return
	}
	if err := s.store.DeleteSession(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			err = &apiError{status: http.StatusNotFound, err: errors.New("session not found")}
		}
		s.fail(w, err)
		return
	}
	noteAudit(r, "session %d", id)
	w.WriteHeader(http.StatusNoContent)
}
