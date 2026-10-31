package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/qwq-inc/gitrepo/internal/gitexec"
	"github.com/qwq-inc/gitrepo/internal/store"
)

// Gitee supports gitee.com (API v5). The credential is a personal access
// token with the projects scope; Git over HTTPS needs the token owner's
// login as the username, which is filled in from the API when the target
// is created (see Identity).
type Gitee struct{}

func (*Gitee) Kind() string { return "gitee" }

// UsesLoginUsername: Gitee authenticates HTTPS pushes as login + token.
func (*Gitee) UsesLoginUsername() bool { return true }

func (*Gitee) Caps() Caps {
	return Caps{CreateRepo: true, BranchProtection: true, DefaultBranch: true}
}

// MaxFileSize is unknown here because it depends on the Gitee plan; set the
// target's max_file_size to enforce it at the hub.
func (*Gitee) MaxFileSize() int64 { return 0 }

func (g *Gitee) web(t *store.Target) string {
	if t.BaseURL == "" {
		return "https://gitee.com"
	}
	return strings.TrimRight(t.BaseURL, "/")
}

func (g *Gitee) api(t *store.Target) string {
	if t.APIURL != "" {
		return strings.TrimRight(t.APIURL, "/")
	}
	return g.web(t) + "/api/v5"
}

func (g *Gitee) Validate(t *store.Target) error {
	if t.Namespace == "" {
		return errors.New("gitee target requires --namespace (user or organization path that owns the mirrors)")
	}
	if t.CredKind != "token" {
		return errors.New("gitee target requires a token credential")
	}
	return nil
}

func (g *Gitee) ns(t *store.Target, spec RepoSpec) string {
	if spec.Namespace != "" {
		return spec.Namespace
	}
	return t.Namespace
}

func (g *Gitee) RepoURL(t *store.Target, namespace, name string) (string, error) {
	if namespace == "" {
		namespace = t.Namespace
	}
	if namespace == "" {
		return "", errors.New("gitee remote needs a namespace")
	}
	return g.web(t) + "/" + namespace + "/" + name + ".git", nil
}

func (g *Gitee) GitCred(t *store.Target, secret string) *gitexec.Cred {
	return &gitexec.Cred{Kind: "token", Username: t.CredUsername, Secret: secret}
}

// call adds the access token, which Gitee expects as a parameter.
func (g *Gitee) call(ctx context.Context, method, u, secret string, in, out any) (int, error) {
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	status, err := doJSON(ctx, method, u+sep+"access_token="+url.QueryEscape(secret), nil, in, out)
	return status, scrub(err, secret)
}

// scrub removes a secret from an error (and from an echoed API body)
// while keeping *APIError intact for status checks.
func scrub(err error, secret string) error {
	if err == nil || secret == "" {
		return err
	}
	var ae *APIError
	if errors.As(err, &ae) {
		ae.Body = strings.ReplaceAll(ae.Body, secret, "***")
		ae.URL = strings.ReplaceAll(ae.URL, secret, "***")
		return err
	}
	if strings.Contains(err.Error(), secret) {
		return errors.New(strings.ReplaceAll(err.Error(), secret, "***"))
	}
	return err
}

func (g *Gitee) repoAPI(t *store.Target, spec RepoSpec) string {
	return g.api(t) + "/repos/" + url.PathEscape(g.ns(t, spec)) + "/" + url.PathEscape(spec.Name)
}

func (g *Gitee) EnsureRepo(ctx context.Context, t *store.Target, secret string, spec RepoSpec) (bool, error) {
	if _, err := g.call(ctx, http.MethodGet, g.repoAPI(t, spec), secret, nil, nil); err == nil {
		return false, nil
	} else if statusOf(err) != http.StatusNotFound {
		return false, err
	}
	owner := g.ns(t, spec)
	createURL := g.api(t) + "/orgs/" + url.PathEscape(owner) + "/repos"
	if who, err := g.Check(ctx, t, secret); err == nil && strings.EqualFold(who, owner) {
		createURL = g.api(t) + "/user/repos"
	}
	body := map[string]any{
		"name":        spec.Name,
		"path":        spec.Name,
		"private":     spec.Visibility != "public",
		"auto_init":   false,
		"description": "Mirror managed by Gitrepo",
	}
	if _, err := g.call(ctx, http.MethodPost, createURL, secret, body, nil); err != nil {
		if st := statusOf(err); (st == http.StatusUnprocessableEntity || st == http.StatusBadRequest) && strings.Contains(err.Error(), "已存在") {
			return false, nil
		}
		return false, fmt.Errorf("create gitee repo %s/%s: %w", owner, spec.Name, err)
	}
	return true, nil
}

// AllowForce removes the protection of branches that must be force-updated
// or deleted; Gitee protected branches accept neither.
func (g *Gitee) AllowForce(ctx context.Context, t *store.Target, secret string, spec RepoSpec, force, del []string) error {
	var errs []error
	for _, b := range append(append([]string{}, force...), del...) {
		branchURL := g.repoAPI(t, spec) + "/branches/" + url.PathEscape(b)
		var info struct {
			Protected bool `json:"protected"`
		}
		if _, err := g.call(ctx, http.MethodGet, branchURL, secret, nil, &info); err != nil {
			if statusOf(err) != http.StatusNotFound {
				errs = append(errs, fmt.Errorf("read branch %s: %w", b, err))
			}
			continue
		}
		if !info.Protected {
			continue
		}
		if _, err := g.call(ctx, http.MethodDelete, branchURL+"/protection", secret, nil, nil); err != nil {
			errs = append(errs, fmt.Errorf("unprotect %s: %w", b, err))
		}
	}
	return errors.Join(errs...)
}

func (g *Gitee) SetDefaultBranch(ctx context.Context, t *store.Target, secret string, spec RepoSpec, branch string) error {
	_, err := g.call(ctx, http.MethodPatch, g.repoAPI(t, spec), secret, map[string]any{"name": spec.Name, "default_branch": branch}, nil)
	return err
}

func (g *Gitee) Check(ctx context.Context, t *store.Target, secret string) (string, error) {
	var me struct {
		Login string `json:"login"`
	}
	if _, err := g.call(ctx, http.MethodGet, g.api(t)+"/user", secret, nil, &me); err != nil {
		return "", err
	}
	return me.Login, nil
}
