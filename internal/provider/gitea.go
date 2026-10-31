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

// Gitea supports Gitea and Forgejo (which shares Gitea's v1 API). The
// credential is an access token with repository read/write scope (plus
// organization write to create repositories in an organization).
type Gitea struct{}

func (*Gitea) Kind() string { return "gitea" }

func (*Gitea) Caps() Caps {
	return Caps{CreateRepo: true, BranchProtection: true, DefaultBranch: true}
}

func (*Gitea) MaxFileSize() int64 { return 0 }

func (g *Gitea) web(t *store.Target) string { return strings.TrimRight(t.BaseURL, "/") }

func (g *Gitea) api(t *store.Target) string {
	if t.APIURL != "" {
		return strings.TrimRight(t.APIURL, "/")
	}
	return g.web(t) + "/api/v1"
}

func (g *Gitea) Validate(t *store.Target) error {
	if t.BaseURL == "" {
		return errors.New("gitea target requires --base-url (e.g. https://git.example.com)")
	}
	if t.Namespace == "" {
		return errors.New("gitea target requires --namespace (user or organization that owns the mirrors)")
	}
	if t.CredKind != "token" {
		return errors.New("gitea target requires a token credential")
	}
	return nil
}

func (g *Gitea) ns(t *store.Target, spec RepoSpec) string {
	if spec.Namespace != "" {
		return spec.Namespace
	}
	return t.Namespace
}

func (g *Gitea) RepoURL(t *store.Target, namespace, name string) (string, error) {
	if namespace == "" {
		namespace = t.Namespace
	}
	if namespace == "" || t.BaseURL == "" {
		return "", errors.New("gitea remote needs base_url and a namespace")
	}
	return g.web(t) + "/" + namespace + "/" + name + ".git", nil
}

// GitCred: Gitea accepts an access token as the password with any username.
func (g *Gitea) GitCred(t *store.Target, secret string) *gitexec.Cred {
	user := t.CredUsername
	if user == "" {
		user = "oauth2"
	}
	return &gitexec.Cred{Kind: "token", Username: user, Secret: secret}
}

func (g *Gitea) headers(secret string) map[string]string {
	return map[string]string{"Authorization": "token " + secret, "Accept": "application/json"}
}

func (g *Gitea) repoAPI(t *store.Target, spec RepoSpec) string {
	return g.api(t) + "/repos/" + url.PathEscape(g.ns(t, spec)) + "/" + url.PathEscape(spec.Name)
}

func (g *Gitea) EnsureRepo(ctx context.Context, t *store.Target, secret string, spec RepoSpec) (bool, error) {
	h := g.headers(secret)
	if _, err := doJSON(ctx, http.MethodGet, g.repoAPI(t, spec), h, nil, nil); err == nil {
		return false, nil
	} else if statusOf(err) != http.StatusNotFound {
		return false, err
	}
	owner := g.ns(t, spec)
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
	if _, err := doJSON(ctx, http.MethodPost, createURL, h, body, nil); err != nil {
		if statusOf(err) == http.StatusConflict {
			return false, nil
		}
		return false, fmt.Errorf("create gitea repo %s/%s: %w", owner, spec.Name, err)
	}
	return true, nil
}

type giteaProtection struct {
	RuleName        string `json:"rule_name"`
	BranchName      string `json:"branch_name"` // Gitea < 1.19
	EnableForcePush *bool  `json:"enable_force_push"`
}

func (p giteaProtection) name() string {
	if p.RuleName != "" {
		return p.RuleName
	}
	return p.BranchName
}

// AllowForce enables force pushes on matching protection rules (Gitea ≥ 1.22
// / Forgejo ≥ 7) and deletes exact-name rules of branches that must be
// deleted. Older servers without enable_force_push get an exact-name rule
// removed instead; pattern rules are never removed.
func (g *Gitea) AllowForce(ctx context.Context, t *store.Target, secret string, spec RepoSpec, force, del []string) error {
	if len(force) == 0 && len(del) == 0 {
		return nil
	}
	h := g.headers(secret)
	base := g.repoAPI(t, spec) + "/branch_protections"
	var rules []giteaProtection
	if _, err := doJSON(ctx, http.MethodGet, base, h, nil, &rules); err != nil {
		if statusOf(err) == http.StatusNotFound {
			return nil
		}
		return err
	}
	var errs []error
	for _, rule := range rules {
		name := rule.name()
		ruleURL := base + "/" + url.PathEscape(name)
		switch {
		case matchesAny(name, del):
			if strings.ContainsAny(name, "*?[") {
				errs = append(errs, fmt.Errorf("cannot delete a branch protected by pattern rule %q; remove the rule or the branch manually", name))
				continue
			}
			if _, err := doJSON(ctx, http.MethodDelete, ruleURL, h, nil, nil); err != nil && statusOf(err) != http.StatusNotFound {
				errs = append(errs, fmt.Errorf("remove protection %s: %w", name, err))
			}
		case matchesAny(name, force):
			if rule.EnableForcePush != nil && *rule.EnableForcePush {
				continue
			}
			var err error
			switch {
			case rule.EnableForcePush != nil:
				_, err = doJSON(ctx, http.MethodPatch, ruleURL, h, map[string]any{"enable_force_push": true}, nil)
			case strings.ContainsAny(name, "*?["):
				err = fmt.Errorf("this server cannot allow force pushes on pattern rule %q; adjust it manually", name)
			default:
				_, err = doJSON(ctx, http.MethodDelete, ruleURL, h, nil, nil)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("allow force push on %s: %w", name, err))
			}
		}
	}
	return errors.Join(errs...)
}

func (g *Gitea) SetDefaultBranch(ctx context.Context, t *store.Target, secret string, spec RepoSpec, branch string) error {
	_, err := doJSON(ctx, http.MethodPatch, g.repoAPI(t, spec), g.headers(secret), map[string]any{"default_branch": branch}, nil)
	return err
}

func (g *Gitea) Check(ctx context.Context, t *store.Target, secret string) (string, error) {
	var me struct {
		Login string `json:"login"`
	}
	if _, err := doJSON(ctx, http.MethodGet, g.api(t)+"/user", g.headers(secret), nil, &me); err != nil {
		return "", err
	}
	return me.Login, nil
}
