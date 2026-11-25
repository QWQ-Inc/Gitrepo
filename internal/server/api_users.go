package server

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/qwq-inc/gitrepo/internal/auth"
	"github.com/qwq-inc/gitrepo/internal/gitserve"
	"github.com/qwq-inc/gitrepo/internal/sso"
	"github.com/qwq-inc/gitrepo/internal/store"
)

// Accounts: a user name and password for the console and for git over
// HTTPS. The first account is created with the bootstrap admin token
// (GITREPO_ADMIN_TOKEN) and is an administrator; the same token resets a
// forgotten password.
func (s *Server) registerUserAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/auth/status", s.apiAuthStatus)
	mux.HandleFunc("POST /api/auth/setup", s.apiAuthSetup)
	mux.HandleFunc("POST /api/auth/login", s.apiAuthLogin)
	mux.HandleFunc("POST /api/auth/reset", s.apiAuthReset)
	mux.HandleFunc("GET /api/me", s.requireAuth(s.apiMe))
	mux.HandleFunc("POST /api/me/password", s.requireAuth(s.apiMyPassword))

	h := func(pattern string, fn http.HandlerFunc) { mux.HandleFunc(pattern, s.requireAdmin(fn)) }
	h("GET /api/users", s.apiListUsers)
	h("POST /api/users", s.apiCreateUser)
	h("PATCH /api/users/{name}", s.apiUpdateUser)
	h("POST /api/users/{name}/password", s.apiSetUserPassword)
	h("DELETE /api/users/{name}", s.apiDeleteUser)
}

// requireAuth admits any authenticated caller (audited like admin calls).
func (s *Server) requireAuth(fn func(http.ResponseWriter, *http.Request, *auth.Principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.auth.FromRequest(r)
		if err != nil || p == nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="gitrepo"`)
			status, msg := http.StatusUnauthorized, "sign in first"
			if errors.Is(err, auth.ErrLocked) {
				status, msg = http.StatusTooManyRequests, err.Error()
			}
			writeErr(w, status, errors.New(msg))
			return
		}
		next := func(w http.ResponseWriter, r *http.Request) { fn(w, r, p) }
		if audited(r) {
			next = s.withAudit(p, next)
		}
		next(w, r)
	}
}

func (s *Server) clientAddr(r *http.Request) string {
	return gitserve.ClientAddr(r, s.cfg.TrustedProxyPrefixes())
}

func (s *Server) auditAuth(r *http.Request, actor, action string, status int, detail string) {
	s.recordAudit(&store.AuditEntry{Actor: actor, Via: "api", ClientAddr: s.clientAddr(r), Action: action, Status: status, Detail: detail})
}

// newUserSession signs an account in.
func (s *Server) newUserSession(ctx context.Context, u *store.User) (*sso.Result, error) {
	plain, hash := auth.NewSessionToken()
	exp := time.Now().Add(s.cfg.SSO.SessionTTL.D())
	x := &store.Session{
		Hash: hash, Provider: store.LocalProvider, Subject: u.Subject(), Name: "user:" + u.Username,
		Scopes: u.Scopes, ExpiresAt: exp.UnixMilli(),
	}
	if err := s.store.CreateSession(ctx, x); err != nil {
		return nil, err
	}
	_ = s.store.TouchUserLogin(ctx, u.ID)
	return &sso.Result{Token: plain, Name: x.Name, ExpiresAt: exp.UTC()}, nil
}

// --------------------------------------------------------------- public

func (s *Server) apiAuthStatus(w http.ResponseWriter, r *http.Request) {
	n, _, err := s.store.CountUsers(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		// No account yet: the console offers to create the first one.
		"needs_setup": n == 0,
		// Whether the bootstrap admin token can create the first account
		// or reset a password.
		"admin_token": s.auth.AdminHash != "",
	})
}

// checkAdminToken verifies the bootstrap admin token in a setup or reset,
// with the same guessing limit as passwords.
func (s *Server) checkAdminToken(r *http.Request, tok string) error {
	key := "addr:" + s.clientAddr(r)
	if s.auth.Limiter.Locked(key) {
		return &apiError{status: http.StatusTooManyRequests, err: auth.ErrLocked}
	}
	if s.auth.AdminHash == "" {
		return &apiError{status: http.StatusForbidden, err: errors.New("no bootstrap admin token is configured (GITREPO_ADMIN_TOKEN); set it and restart")}
	}
	if !s.auth.IsAdminToken(strings.TrimSpace(tok)) {
		s.auth.Limiter.Fail(key)
		return &apiError{status: http.StatusUnauthorized, err: errors.New("the admin token is wrong")}
	}
	return nil
}

func (s *Server) apiAuthSetup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AdminToken string `json:"admin_token"`
		Username   string `json:"username"`
		Password   string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.checkAdminToken(r, in.AdminToken); err != nil {
		s.auditAuth(r, "setup", "create first account", http.StatusUnauthorized, "refused: "+err.Error())
		s.fail(w, err)
		return
	}
	name, err := auth.NormalizeUsername(in.Username)
	if err != nil {
		s.fail(w, badRequest("%v", err))
		return
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		s.fail(w, badRequest("%v", err))
		return
	}
	u := &store.User{Username: name, PasswordHash: hash, Scopes: []string{auth.ScopeAdmin}}
	if err := s.store.CreateFirstUser(r.Context(), u); err != nil {
		if errors.Is(err, store.ErrConflict) {
			err = &apiError{status: http.StatusConflict, err: errors.New("an account exists already; sign in, or reset its password with the admin token")}
		}
		s.fail(w, err)
		return
	}
	res, err := s.newUserSession(r.Context(), u)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditAuth(r, "user:"+name, "create first account", http.StatusCreated, "administrator "+name+", with the bootstrap admin token")
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) apiAuthLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	name := strings.ToLower(strings.TrimSpace(in.Username))
	u, err := s.auth.Password(r.Context(), name, in.Password, s.clientAddr(r))
	switch {
	case errors.Is(err, auth.ErrLocked):
		s.auditAuth(r, "user:"+name, "sign in", http.StatusTooManyRequests, "refused: locked after repeated failures")
		s.fail(w, &apiError{status: http.StatusTooManyRequests, err: err})
		return
	case errors.Is(err, auth.ErrInvalidToken):
		s.auditAuth(r, "user:"+name, "sign in", http.StatusUnauthorized, "refused: wrong user name or password")
		s.fail(w, &apiError{status: http.StatusUnauthorized, err: errors.New("wrong user name or password")})
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	res, err := s.newUserSession(r.Context(), u)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditAuth(r, "user:"+u.Username, "sign in", http.StatusOK, "signed in with a password")
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, res)
}

// apiAuthReset sets a forgotten password, proven by the bootstrap admin
// token. The account's sessions end; a disabled account stays disabled.
func (s *Server) apiAuthReset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AdminToken string `json:"admin_token"`
		Username   string `json:"username"`
		Password   string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	name := strings.ToLower(strings.TrimSpace(in.Username))
	if err := s.checkAdminToken(r, in.AdminToken); err != nil {
		s.auditAuth(r, "user:"+name, "reset password", http.StatusUnauthorized, "refused: "+err.Error())
		s.fail(w, err)
		return
	}
	u, err := s.store.GetUser(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		err = &apiError{status: http.StatusNotFound, err: errors.New("no such account")}
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		s.fail(w, badRequest("%v", err))
		return
	}
	if err := s.store.SetPassword(r.Context(), u.ID, hash); err != nil {
		s.fail(w, err)
		return
	}
	s.auth.Limiter.Reset("user:" + u.Username)
	s.auditAuth(r, "user:"+u.Username, "reset password", http.StatusOK, "with the bootstrap admin token; sessions ended")
	w.WriteHeader(http.StatusNoContent)
}

// ------------------------------------------------------------ signed in

type meOut struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind"` // admin-token | token | user | sso
	Username string   `json:"username,omitempty"`
	Admin    bool     `json:"admin"`
	Scopes   []string `json:"scopes"`
	Repos    []string `json:"repos,omitempty"`
}

func (s *Server) apiMe(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	out := meOut{Name: p.Name, Admin: p.IsAdmin(), Scopes: p.Scopes, Repos: p.Repos, Kind: "token"}
	switch {
	case p.UserID != 0:
		out.Kind, out.Username = "user", strings.TrimPrefix(p.Name, "user:")
	case p.SessionID != 0:
		out.Kind = "sso"
	case p.Name == auth.Admin().Name:
		out.Kind = "admin-token"
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiMyPassword(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var in struct {
		Current  string `json:"current"`
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	if p.UserID == 0 {
		s.fail(w, badRequest("only an account has a password"))
		return
	}
	u, err := s.store.GetUserByID(r.Context(), p.UserID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if _, err := s.auth.Password(r.Context(), u.Username, in.Current, s.clientAddr(r)); err != nil {
		if errors.Is(err, auth.ErrLocked) {
			s.fail(w, &apiError{status: http.StatusTooManyRequests, err: err})
		} else {
			s.fail(w, &apiError{status: http.StatusForbidden, err: errors.New("the current password is wrong")})
		}
		return
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		s.fail(w, badRequest("%v", err))
		return
	}
	if err := s.store.SetPassword(r.Context(), u.ID, hash); err != nil {
		s.fail(w, err)
		return
	}
	// Every session ended, this one too: sign in again with the new
	// password (the console does it for the person).
	res, err := s.newUserSession(r.Context(), u)
	if err != nil {
		s.fail(w, err)
		return
	}
	noteAudit(r, "changed own password; other sessions ended")
	writeJSON(w, http.StatusOK, res)
}

// ------------------------------------------------------------------ admin

type userOut struct {
	Username    string     `json:"username"`
	Admin       bool       `json:"admin"`
	Scopes      []string   `json:"scopes"`
	Repos       []string   `json:"repos,omitempty"`
	Disabled    bool       `json:"disabled"`
	CreatedAt   *time.Time `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
	PasswordAt  *time.Time `json:"password_changed_at,omitempty"`
}

func toUserOut(u *store.User) userOut {
	return userOut{Username: u.Username, Admin: slices.Contains(u.Scopes, auth.ScopeAdmin), Scopes: u.Scopes, Repos: u.Repos,
		Disabled: u.Disabled, CreatedAt: ts(u.CreatedAt), LastLoginAt: ts(u.LastLoginAt), PasswordAt: ts(u.PasswordChangedAt)}
}

func (s *Server) apiListUsers(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListUsers(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []userOut{}
	for _, u := range list {
		out = append(out, toUserOut(u))
	}
	writeJSON(w, http.StatusOK, out)
}

// userScopes turns the request's admin flag or scopes into a scope list.
func userScopes(admin *bool, scopes *[]string, repos *[]string, u *store.User) error {
	if admin != nil && *admin {
		u.Scopes, u.Repos = []string{auth.ScopeAdmin}, nil
		return nil
	}
	if scopes != nil {
		sc, err := auth.ParseScopes(strings.Join(*scopes, ","))
		if err != nil {
			return badRequest("%v", err)
		}
		if slices.Contains(sc, auth.ScopeEdge) {
			return badRequest("accounts cannot have the edge scope; create a token for an edge node")
		}
		u.Scopes = sc
	} else if admin != nil && !*admin && slices.Contains(u.Scopes, auth.ScopeAdmin) {
		u.Scopes = []string{auth.ScopeCreate}
	}
	if repos != nil {
		rp, err := auth.ParseRepoPatterns(strings.Join(*repos, ","))
		if err != nil {
			return badRequest("%v", err)
		}
		u.Repos = rp
	}
	if slices.Contains(u.Scopes, auth.ScopeAdmin) {
		u.Repos = nil // an administrator has every repository
	}
	return nil
}

func (s *Server) apiCreateUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string    `json:"username"`
		Password string    `json:"password"`
		Admin    *bool     `json:"admin"`
		Scopes   *[]string `json:"scopes"`
		Repos    *[]string `json:"repos"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	name, err := auth.NormalizeUsername(in.Username)
	if err != nil {
		s.fail(w, badRequest("%v", err))
		return
	}
	u := &store.User{Username: name, Scopes: []string{auth.ScopeCreate}}
	if err := userScopes(in.Admin, in.Scopes, in.Repos, u); err != nil {
		s.fail(w, err)
		return
	}
	if u.PasswordHash, err = auth.HashPassword(in.Password); err != nil {
		s.fail(w, badRequest("%v", err))
		return
	}
	if err := s.store.CreateUser(r.Context(), u); err != nil {
		if errors.Is(err, store.ErrConflict) {
			err = &apiError{status: http.StatusConflict, err: errors.New("an account with this name exists")}
		}
		s.fail(w, err)
		return
	}
	noteAudit(r, "account %s (scopes %s; repos %s)", u.Username, strings.Join(u.Scopes, ","), orAll(u.Repos))
	writeJSON(w, http.StatusCreated, toUserOut(u))
}

func (s *Server) userFromPath(r *http.Request) (*store.User, error) {
	u, err := s.store.GetUser(r.Context(), strings.ToLower(r.PathValue("name")))
	if errors.Is(err, store.ErrNotFound) {
		return nil, &apiError{status: http.StatusNotFound, err: errors.New("no such account")}
	}
	return u, err
}

// keepAnAdmin refuses a change that would leave no enabled administrator
// account (the bootstrap token could still reset a password, but the
// console would have nobody to sign in as).
func (s *Server) keepAnAdmin(ctx context.Context, before, after *store.User) error {
	wasAdmin := !before.Disabled && slices.Contains(before.Scopes, auth.ScopeAdmin)
	staysAdmin := after != nil && !after.Disabled && slices.Contains(after.Scopes, auth.ScopeAdmin)
	if !wasAdmin || staysAdmin {
		return nil
	}
	_, admins, err := s.store.CountUsers(ctx)
	if err != nil {
		return err
	}
	if admins <= 1 {
		return &apiError{status: http.StatusConflict, err: errors.New("this is the last enabled administrator account; make another account an administrator first")}
	}
	return nil
}

func (s *Server) apiUpdateUser(w http.ResponseWriter, r *http.Request) {
	u, err := s.userFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	var in struct {
		Admin    *bool     `json:"admin"`
		Scopes   *[]string `json:"scopes"`
		Repos    *[]string `json:"repos"`
		Disabled *bool     `json:"disabled"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	before := *u
	if err := userScopes(in.Admin, in.Scopes, in.Repos, u); err != nil {
		s.fail(w, err)
		return
	}
	if in.Disabled != nil {
		u.Disabled = *in.Disabled
	}
	if err := s.keepAnAdmin(r.Context(), &before, u); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.UpdateUser(r.Context(), u); err != nil {
		s.fail(w, err)
		return
	}
	noteAudit(r, "account %s (scopes %s; repos %s; disabled %t); sessions ended", u.Username, strings.Join(u.Scopes, ","), orAll(u.Repos), u.Disabled)
	writeJSON(w, http.StatusOK, toUserOut(u))
}

func (s *Server) apiSetUserPassword(w http.ResponseWriter, r *http.Request) {
	u, err := s.userFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		s.fail(w, badRequest("%v", err))
		return
	}
	if err := s.store.SetPassword(r.Context(), u.ID, hash); err != nil {
		s.fail(w, err)
		return
	}
	s.auth.Limiter.Reset("user:" + u.Username)
	noteAudit(r, "password of account %s set; sessions ended", u.Username)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiDeleteUser(w http.ResponseWriter, r *http.Request) {
	u, err := s.userFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.keepAnAdmin(r.Context(), u, nil); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.DeleteUser(r.Context(), u.ID); err != nil {
		s.fail(w, err)
		return
	}
	noteAudit(r, "account %s", u.Username)
	w.WriteHeader(http.StatusNoContent)
}
