package provider

import (
	"context"
	"errors"
	"strings"

	"github.com/qwq-inc/gitrepo/internal/gitexec"
	"github.com/qwq-inc/gitrepo/internal/store"
)

// Generic is any Git server reachable over HTTPS or SSH without a supported
// API (gitolite, plain sshd, Gitea/Forgejo until their adapters land, …).
// Repositories must already exist; Gitrepo only pushes.
//
// The URL comes from the target's url_template, where {namespace} and {name}
// are substituted, e.g. "https://git.example.com/{namespace}/{name}.git" or
// "ssh://git@git.example.com/{namespace}/{name}.git". Without a template the
// URL is base_url/namespace/name.git.
type Generic struct{}

func (*Generic) Kind() string       { return "generic" }
func (*Generic) Caps() Caps         { return Caps{} }
func (*Generic) MaxFileSize() int64 { return 0 }

func (g *Generic) Validate(t *store.Target) error {
	if t.URLTemplate == "" && t.BaseURL == "" {
		return errors.New("generic target requires --url-template or --base-url")
	}
	if t.URLTemplate != "" && !strings.Contains(t.URLTemplate, "{name}") {
		return errors.New("url template must contain {name}")
	}
	switch t.CredKind {
	case "none", "token", "basic", "ssh":
	default:
		return errors.New("generic credential kind must be none, token, basic or ssh")
	}
	return nil
}

func (g *Generic) RepoURL(t *store.Target, namespace, name string) (string, error) {
	if namespace == "" {
		namespace = t.Namespace
	}
	if t.URLTemplate != "" {
		u := strings.ReplaceAll(t.URLTemplate, "{namespace}", namespace)
		u = strings.ReplaceAll(u, "{name}", name)
		// Collapse "//" left behind by an empty namespace, keeping the scheme.
		if i := strings.Index(u, "://"); i >= 0 {
			u = u[:i+3] + collapseSlashes(u[i+3:])
		}
		return u, nil
	}
	if t.BaseURL == "" {
		return "", errors.New("generic target has neither url_template nor base_url")
	}
	return strings.TrimRight(t.BaseURL, "/") + "/" + joinPath(namespace, name) + ".git", nil
}

func collapseSlashes(s string) string {
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	return s
}

func (g *Generic) GitCred(t *store.Target, secret string) *gitexec.Cred {
	switch t.CredKind {
	case "token":
		user := t.CredUsername
		if user == "" {
			user = "git"
		}
		return &gitexec.Cred{Kind: "token", Username: user, Secret: secret}
	case "basic":
		return &gitexec.Cred{Kind: "basic", Username: t.CredUsername, Secret: secret}
	case "ssh":
		return &gitexec.Cred{Kind: "ssh", Secret: secret, KnownHosts: t.SSHKnownHosts}
	default:
		return nil
	}
}

func (g *Generic) EnsureRepo(context.Context, *store.Target, string, RepoSpec) (bool, error) {
	return false, nil
}

func (g *Generic) AllowForce(context.Context, *store.Target, string, RepoSpec, []string, []string) error {
	return nil
}

func (g *Generic) SetDefaultBranch(context.Context, *store.Target, string, RepoSpec, string) error {
	return nil
}

func (g *Generic) Check(context.Context, *store.Target, string) (string, error) {
	return "", errors.New("generic targets have no API; credentials are verified on the first sync")
}
