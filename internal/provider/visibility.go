package provider

import (
	"context"
	"net/http"
	"net/url"

	"github.com/qwq-inc/gitrepo/internal/store"
)

// VisibilitySetter is implemented by providers that can change a mirror
// repository's visibility (private, internal or public).
type VisibilitySetter interface {
	SetVisibility(ctx context.Context, t *store.Target, secret string, spec RepoSpec, visibility string) error
}

func (g *GitHub) SetVisibility(ctx context.Context, t *store.Target, secret string, spec RepoSpec, visibility string) error {
	body := map[string]any{"private": visibility != "public"}
	if visibility == "internal" {
		body = map[string]any{"visibility": "internal"} // GitHub Enterprise organizations
	}
	u := g.api(t) + "/repos/" + url.PathEscape(g.ns(t, spec)) + "/" + url.PathEscape(spec.Name)
	_, err := doJSON(ctx, http.MethodPatch, u, g.headers(secret), body, nil)
	return err
}

func (g *GitLab) SetVisibility(ctx context.Context, t *store.Target, secret string, spec RepoSpec, visibility string) error {
	_, err := doJSON(ctx, http.MethodPut, g.projectURL(t, spec), g.headers(secret), map[string]any{"visibility": visibility}, nil)
	return err
}

// Gitea and Gitee know only private or public; internal becomes private.
func (g *Gitea) SetVisibility(ctx context.Context, t *store.Target, secret string, spec RepoSpec, visibility string) error {
	_, err := doJSON(ctx, http.MethodPatch, g.repoAPI(t, spec), g.headers(secret), map[string]any{"private": visibility != "public"}, nil)
	return err
}

func (g *Gitee) SetVisibility(ctx context.Context, t *store.Target, secret string, spec RepoSpec, visibility string) error {
	_, err := g.call(ctx, http.MethodPatch, g.repoAPI(t, spec), secret, map[string]any{"name": spec.Name, "private": visibility != "public"}, nil)
	return err
}

var (
	_ VisibilitySetter = (*GitHub)(nil)
	_ VisibilitySetter = (*GitLab)(nil)
	_ VisibilitySetter = (*Gitea)(nil)
	_ VisibilitySetter = (*Gitee)(nil)
)
