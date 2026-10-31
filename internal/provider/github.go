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

// GitHub supports github.com and GitHub Enterprise Server. The credential is
// a fine-grained/classic PAT or a GitHub App installation token with
// contents:write (plus administration:write to create repositories).
type GitHub struct{}

func (*GitHub) Kind() string { return "github" }

func (*GitHub) Caps() Caps {
	return Caps{CreateRepo: true, BranchProtection: false, DefaultBranch: true}
}

// GitHub rejects files larger than 100 MiB.
func (*GitHub) MaxFileSize() int64 { return 100 << 20 }

func (g *GitHub) web(t *store.Target) string {
	if t.BaseURL == "" {
		return "https://github.com"
	}
	return strings.TrimRight(t.BaseURL, "/")
}

func (g *GitHub) api(t *store.Target) string {
	if t.APIURL != "" {
		return strings.TrimRight(t.APIURL, "/")
	}
	web := g.web(t)
	if web == "https://github.com" {
		return "https://api.github.com"
	}
	return web + "/api/v3"
}

func (g *GitHub) Validate(t *store.Target) error {
	if t.Namespace == "" {
		return errors.New("github target requires --namespace (user or organization that owns the mirrors)")
	}
	if t.CredKind != "token" {
		return errors.New("github target requires a token credential")
	}
	return nil
}

func (g *GitHub) RepoURL(t *store.Target, namespace, name string) (string, error) {
	if namespace == "" {
		namespace = t.Namespace
	}
	if namespace == "" {
		return "", errors.New("github remote needs a namespace")
	}
	return g.web(t) + "/" + namespace + "/" + name + ".git", nil
}

func (g *GitHub) GitCred(t *store.Target, secret string) *gitexec.Cred {
	return &gitexec.Cred{Kind: "token", Username: "x-access-token", Secret: secret}
}

func (g *GitHub) headers(secret string) map[string]string {
	return map[string]string{
		"Authorization":        "Bearer " + secret,
		"Accept":               "application/vnd.github+json",
		"X-GitHub-Api-Version": "2022-11-28",
	}
}

func (g *GitHub) ns(t *store.Target, spec RepoSpec) string {
	if spec.Namespace != "" {
		return spec.Namespace
	}
	return t.Namespace
}

func (g *GitHub) EnsureRepo(ctx context.Context, t *store.Target, secret string, spec RepoSpec) (bool, error) {
	owner := g.ns(t, spec)
	h := g.headers(secret)
	repoURL := g.api(t) + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(spec.Name)
	if _, err := doJSON(ctx, http.MethodGet, repoURL, h, nil, nil); err == nil {
		return false, nil
	} else if statusOf(err) != http.StatusNotFound {
		return false, err
	}

	// Personal account or organization?
	createURL := g.api(t) + "/orgs/" + url.PathEscape(owner) + "/repos"
	var me struct {
		Login string `json:"login"`
	}
	if _, err := doJSON(ctx, http.MethodGet, g.api(t)+"/user", h, nil, &me); err == nil && strings.EqualFold(me.Login, owner) {
		createURL = g.api(t) + "/user/repos"
	}
	body := map[string]any{
		"name":        spec.Name,
		"private":     spec.Visibility != "public",
		"auto_init":   false,
		"description": "Mirror managed by repohub",
	}
	if spec.Visibility == "internal" {
		body["visibility"] = "internal"
	}
	if _, err := doJSON(ctx, http.MethodPost, createURL, h, body, nil); err != nil {
		if statusOf(err) == http.StatusUnprocessableEntity && strings.Contains(err.Error(), "already exists") {
			return false, nil
		}
		return false, fmt.Errorf("create github repo %s/%s: %w", owner, spec.Name, err)
	}
	return true, nil
}

// AllowForce is a no-op: GitHub has no default branch protection, and
// user-defined rules on mirrors are respected rather than removed.
func (g *GitHub) AllowForce(ctx context.Context, t *store.Target, secret string, spec RepoSpec, force, del []string) error {
	return nil
}

func (g *GitHub) SetDefaultBranch(ctx context.Context, t *store.Target, secret string, spec RepoSpec, branch string) error {
	owner := g.ns(t, spec)
	u := g.api(t) + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(spec.Name)
	_, err := doJSON(ctx, http.MethodPatch, u, g.headers(secret), map[string]any{"default_branch": branch}, nil)
	return err
}

func (g *GitHub) Check(ctx context.Context, t *store.Target, secret string) (string, error) {
	var me struct {
		Login string `json:"login"`
	}
	if _, err := doJSON(ctx, http.MethodGet, g.api(t)+"/user", g.headers(secret), nil, &me); err != nil {
		return "", err
	}
	return me.Login, nil
}
