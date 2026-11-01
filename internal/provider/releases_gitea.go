package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/qwq-inc/gitrepo/internal/store"
)

type giteaRelease struct {
	ID     json.Number `json:"id"`
	Assets []struct {
		ID   json.Number `json:"id"`
		Name string      `json:"name"`
		Size int64       `json:"size"`
	} `json:"assets"`
}

func (r *giteaRelease) remote() *RemoteRelease {
	rr := &RemoteRelease{ID: idString(r.ID), Key: idString(r.ID)}
	for _, a := range r.Assets {
		rr.Assets = append(rr.Assets, RemoteAsset{ID: idString(a.ID), Name: a.Name, Size: a.Size})
	}
	return rr
}

func (g *Gitea) GetRelease(ctx context.Context, t *store.Target, secret string, spec RepoSpec, tag string) (*RemoteRelease, error) {
	var r giteaRelease
	// One path segment: Gitea routes the tag as a single parameter.
	if _, err := doJSON(ctx, http.MethodGet, g.repoAPI(t, spec)+"/releases/tags/"+url.PathEscape(tag), g.headers(secret), nil, &r); err != nil {
		if statusOf(err) == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	return r.remote(), nil
}

func (g *Gitea) CreateRelease(ctx context.Context, t *store.Target, secret string, spec RepoSpec, rel Release) (*RemoteRelease, error) {
	var r giteaRelease
	body := map[string]any{"tag_name": rel.Tag, "name": rel.Name, "body": rel.Body, "prerelease": rel.Prerelease, "draft": false}
	if rel.Commit != "" {
		body["target_commitish"] = rel.Commit
	}
	if _, err := doJSON(ctx, http.MethodPost, g.repoAPI(t, spec)+"/releases", g.headers(secret), body, &r); err != nil {
		return nil, err
	}
	return r.remote(), nil
}

func (g *Gitea) UpdateRelease(ctx context.Context, t *store.Target, secret string, spec RepoSpec, rr *RemoteRelease, rel Release) error {
	body := map[string]any{"name": rel.Name, "body": rel.Body, "prerelease": rel.Prerelease}
	_, err := doJSON(ctx, http.MethodPatch, g.repoAPI(t, spec)+"/releases/"+url.PathEscape(rr.ID), g.headers(secret), body, nil)
	return err
}

func (g *Gitea) DeleteRelease(ctx context.Context, t *store.Target, secret string, spec RepoSpec, rr *RemoteRelease) error {
	_, err := doJSON(ctx, http.MethodDelete, g.repoAPI(t, spec)+"/releases/"+url.PathEscape(rr.ID), g.headers(secret), nil, nil)
	if statusOf(err) == http.StatusNotFound {
		return nil
	}
	return err
}

func (g *Gitea) UploadAsset(ctx context.Context, t *store.Target, secret string, spec RepoSpec, rr *RemoteRelease, a Asset) (*RemoteAsset, error) {
	var out struct {
		ID   json.Number `json:"id"`
		Name string      `json:"name"`
		Size int64       `json:"size"`
	}
	u := g.repoAPI(t, spec) + "/releases/" + url.PathEscape(rr.ID) + "/assets?name=" + url.QueryEscape(a.Name)
	if err := uploadMultipart(ctx, u, g.headers(secret), "attachment", a, &out); err != nil {
		return nil, err
	}
	return &RemoteAsset{ID: idString(out.ID), Name: out.Name, Size: out.Size}, nil
}

func (g *Gitea) DeleteAsset(ctx context.Context, t *store.Target, secret string, spec RepoSpec, rr *RemoteRelease, assetID string) error {
	_, err := doJSON(ctx, http.MethodDelete, g.repoAPI(t, spec)+"/releases/"+url.PathEscape(rr.ID)+"/assets/"+url.PathEscape(assetID), g.headers(secret), nil, nil)
	if statusOf(err) == http.StatusNotFound {
		return nil
	}
	return err
}
