package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/qwq-inc/gitrepo/internal/gitexec"
	"github.com/qwq-inc/gitrepo/internal/store"
)

// GitLab supports gitlab.com and self-managed GitLab. The credential is a
// personal, group or project access token with api + write_repository.
type GitLab struct{}

func (*GitLab) Kind() string { return "gitlab" }

func (*GitLab) Caps() Caps {
	return Caps{CreateRepo: true, BranchProtection: true, DefaultBranch: true}
}

func (*GitLab) MaxFileSize() int64 { return 0 }

func (g *GitLab) web(t *store.Target) string {
	if t.BaseURL == "" {
		return "https://gitlab.com"
	}
	return strings.TrimRight(t.BaseURL, "/")
}

func (g *GitLab) api(t *store.Target) string {
	if t.APIURL != "" {
		return strings.TrimRight(t.APIURL, "/")
	}
	return g.web(t) + "/api/v4"
}

func (g *GitLab) Validate(t *store.Target) error {
	if t.Namespace == "" {
		return errors.New("gitlab target requires --namespace (user or group path that owns the mirrors)")
	}
	if t.CredKind != "token" {
		return errors.New("gitlab target requires a token credential")
	}
	return nil
}

func (g *GitLab) ns(t *store.Target, spec RepoSpec) string {
	if spec.Namespace != "" {
		return spec.Namespace
	}
	return t.Namespace
}

func (g *GitLab) RepoURL(t *store.Target, namespace, name string) (string, error) {
	if namespace == "" {
		namespace = t.Namespace
	}
	if namespace == "" {
		return "", errors.New("gitlab remote needs a namespace")
	}
	return g.web(t) + "/" + joinPath(namespace, name) + ".git", nil
}

func (g *GitLab) GitCred(t *store.Target, secret string) *gitexec.Cred {
	return &gitexec.Cred{Kind: "token", Username: "oauth2", Secret: secret}
}

func (g *GitLab) headers(secret string) map[string]string {
	return map[string]string{"PRIVATE-TOKEN": secret}
}

func (g *GitLab) projectURL(t *store.Target, spec RepoSpec) string {
	return g.api(t) + "/projects/" + url.PathEscape(joinPath(g.ns(t, spec), spec.Name))
}

func (g *GitLab) EnsureRepo(ctx context.Context, t *store.Target, secret string, spec RepoSpec) (bool, error) {
	h := g.headers(secret)
	if _, err := doJSON(ctx, http.MethodGet, g.projectURL(t, spec), h, nil, nil); err == nil {
		return false, nil
	} else if statusOf(err) != http.StatusNotFound {
		return false, err
	}
	ns := g.ns(t, spec)
	var nsInfo struct {
		ID int64 `json:"id"`
	}
	if _, err := doJSON(ctx, http.MethodGet, g.api(t)+"/namespaces/"+url.PathEscape(ns), h, nil, &nsInfo); err != nil {
		return false, fmt.Errorf("look up gitlab namespace %q: %w", ns, err)
	}
	vis := spec.Visibility
	if vis == "" {
		vis = "private"
	}
	body := map[string]any{
		"name":                   spec.Name,
		"path":                   spec.Name,
		"namespace_id":           nsInfo.ID,
		"visibility":             vis,
		"initialize_with_readme": false,
		"description":            "Mirror managed by repohub",
	}
	if _, err := doJSON(ctx, http.MethodPost, g.api(t)+"/projects", h, body, nil); err != nil {
		if statusOf(err) == http.StatusBadRequest && strings.Contains(err.Error(), "has already been taken") {
			return false, nil
		}
		return false, fmt.Errorf("create gitlab project %s/%s: %w", ns, spec.Name, err)
	}
	return true, nil
}

type glProtected struct {
	Name           string `json:"name"`
	AllowForcePush bool   `json:"allow_force_push"`
}

// AllowForce enables allow_force_push on protection rules matching branches
// that must be force-updated (the mirror is managed by repohub, so this is
// left in place), and removes exact-name rules of branches that must be
// deleted (GitLab never lets a push delete a protected branch). Wildcard
// rules are never removed.
func (g *GitLab) AllowForce(ctx context.Context, t *store.Target, secret string, spec RepoSpec, force, del []string) error {
	if len(force) == 0 && len(del) == 0 {
		return nil
	}
	h := g.headers(secret)
	base := g.projectURL(t, spec) + "/protected_branches"
	var rules []glProtected
	for page := 1; page <= 20; page++ {
		var batch []glProtected
		if _, err := doJSON(ctx, http.MethodGet, fmt.Sprintf("%s?per_page=100&page=%d", base, page), h, nil, &batch); err != nil {
			if statusOf(err) == http.StatusNotFound {
				return nil
			}
			return err
		}
		rules = append(rules, batch...)
		if len(batch) < 100 {
			break
		}
	}
	var errs []error
	for _, rule := range rules {
		ruleURL := base + "/" + url.PathEscape(rule.Name)
		if matchesAny(rule.Name, del) {
			if strings.Contains(rule.Name, "*") {
				// Removing a wildcard rule would unprotect every branch it
				// covers; leave that decision to a human.
				errs = append(errs, fmt.Errorf("cannot delete a branch protected by wildcard rule %q; remove the rule or the branch manually", rule.Name))
				continue
			}
			if _, err := doJSON(ctx, http.MethodDelete, ruleURL, h, nil, nil); err != nil && statusOf(err) != http.StatusNotFound {
				errs = append(errs, fmt.Errorf("unprotect %s: %w", rule.Name, err))
			}
			continue
		}
		if rule.AllowForcePush || !matchesAny(rule.Name, force) {
			continue
		}
		_, err := doJSON(ctx, http.MethodPatch, ruleURL, h, map[string]any{"allow_force_push": true}, nil)
		if st := statusOf(err); st == http.StatusNotFound || st == http.StatusMethodNotAllowed {
			// GitLab < 15.6 cannot update rules in place: re-create it.
			if _, err = doJSON(ctx, http.MethodDelete, ruleURL, h, nil, nil); err == nil {
				_, err = doJSON(ctx, http.MethodPost, base, h, map[string]any{"name": rule.Name, "allow_force_push": true}, nil)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("allow force push on %s: %w", rule.Name, err))
		}
	}
	return errors.Join(errs...)
}

// matchesAny reports whether a GitLab protection rule name (which may contain
// '*' wildcards) matches any of the branches.
func matchesAny(rule string, branches []string) bool {
	if len(branches) == 0 {
		return false
	}
	var re *regexp.Regexp
	if strings.Contains(rule, "*") {
		parts := strings.Split(rule, "*")
		for i, p := range parts {
			parts[i] = regexp.QuoteMeta(p)
		}
		re = regexp.MustCompile("^" + strings.Join(parts, ".*") + "$")
	}
	for _, b := range branches {
		if b == rule || (re != nil && re.MatchString(b)) {
			return true
		}
	}
	return false
}

func (g *GitLab) SetDefaultBranch(ctx context.Context, t *store.Target, secret string, spec RepoSpec, branch string) error {
	_, err := doJSON(ctx, http.MethodPut, g.projectURL(t, spec), g.headers(secret), map[string]any{"default_branch": branch}, nil)
	return err
}

func (g *GitLab) Check(ctx context.Context, t *store.Target, secret string) (string, error) {
	var me struct {
		Username string `json:"username"`
	}
	if _, err := doJSON(ctx, http.MethodGet, g.api(t)+"/user", g.headers(secret), nil, &me); err != nil {
		return "", err
	}
	return me.Username, nil
}
