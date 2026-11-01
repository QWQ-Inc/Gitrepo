package provider

import (
	"context"
	"net/http"
	"net/url"

	"github.com/qwq-inc/gitrepo/internal/store"
)

// RepoDeleter is implemented by providers that can delete a repository.
// Gitrepo never deletes mirrors; it is used only to clean up the scratch
// repository of a write check (gr target check --write).
type RepoDeleter interface {
	DeleteRepo(ctx context.Context, t *store.Target, secret string, spec RepoSpec) error
}

func (g *GitHub) DeleteRepo(ctx context.Context, t *store.Target, secret string, spec RepoSpec) error {
	u := g.api(t) + "/repos/" + url.PathEscape(g.ns(t, spec)) + "/" + url.PathEscape(spec.Name)
	_, err := doJSON(ctx, http.MethodDelete, u, g.headers(secret), nil, nil)
	return err
}

func (g *GitLab) DeleteRepo(ctx context.Context, t *store.Target, secret string, spec RepoSpec) error {
	_, err := doJSON(ctx, http.MethodDelete, g.projectURL(t, spec), g.headers(secret), nil, nil)
	return err
}

func (g *Gitea) DeleteRepo(ctx context.Context, t *store.Target, secret string, spec RepoSpec) error {
	_, err := doJSON(ctx, http.MethodDelete, g.repoAPI(t, spec), g.headers(secret), nil, nil)
	return err
}

func (g *Gitee) DeleteRepo(ctx context.Context, t *store.Target, secret string, spec RepoSpec) error {
	_, err := g.call(ctx, http.MethodDelete, g.repoAPI(t, spec), secret, nil, nil)
	return err
}
