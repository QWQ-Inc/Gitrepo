package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/qwq-inc/gitrepo/internal/store"
)

// GitLab release assets are links; files are uploaded to the project
// (/uploads) and linked from the release. GitLab has no pre-release flag.

type glRelease struct {
	TagName string `json:"tag_name"`
	Assets  struct {
		Links []struct {
			ID   json.Number `json:"id"`
			Name string      `json:"name"`
		} `json:"links"`
	} `json:"assets"`
}

func (r *glRelease) remote() *RemoteRelease {
	rr := &RemoteRelease{ID: r.TagName}
	for _, l := range r.Assets.Links {
		rr.Assets = append(rr.Assets, RemoteAsset{ID: idString(l.ID), Name: l.Name, Size: -1})
	}
	return rr
}

func (g *GitLab) releaseURL(t *store.Target, spec RepoSpec, tag string) string {
	return g.projectURL(t, spec) + "/releases/" + url.PathEscape(tag)
}

func (g *GitLab) GetRelease(ctx context.Context, t *store.Target, secret string, spec RepoSpec, tag string) (*RemoteRelease, error) {
	var r glRelease
	if _, err := doJSON(ctx, http.MethodGet, g.releaseURL(t, spec, tag), g.headers(secret), nil, &r); err != nil {
		if statusOf(err) == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	if r.TagName == "" {
		r.TagName = tag
	}
	return r.remote(), nil
}

func (g *GitLab) CreateRelease(ctx context.Context, t *store.Target, secret string, spec RepoSpec, rel Release) (*RemoteRelease, error) {
	var r glRelease
	body := map[string]any{"tag_name": rel.Tag, "name": rel.Name, "description": rel.Body}
	if _, err := doJSON(ctx, http.MethodPost, g.projectURL(t, spec)+"/releases", g.headers(secret), body, &r); err != nil {
		return nil, err
	}
	if r.TagName == "" {
		r.TagName = rel.Tag
	}
	return r.remote(), nil
}

func (g *GitLab) UpdateRelease(ctx context.Context, t *store.Target, secret string, spec RepoSpec, rr *RemoteRelease, rel Release) error {
	body := map[string]any{"name": rel.Name, "description": rel.Body}
	_, err := doJSON(ctx, http.MethodPut, g.releaseURL(t, spec, rr.ID), g.headers(secret), body, nil)
	return err
}

func (g *GitLab) DeleteRelease(ctx context.Context, t *store.Target, secret string, spec RepoSpec, rr *RemoteRelease) error {
	_, err := doJSON(ctx, http.MethodDelete, g.releaseURL(t, spec, rr.ID), g.headers(secret), nil, nil)
	if statusOf(err) == http.StatusNotFound {
		return nil
	}
	return err
}

func (g *GitLab) UploadAsset(ctx context.Context, t *store.Target, secret string, spec RepoSpec, rr *RemoteRelease, a Asset) (*RemoteAsset, error) {
	var up struct {
		URL      string `json:"url"`
		FullPath string `json:"full_path"`
	}
	if err := uploadMultipart(ctx, g.projectURL(t, spec)+"/uploads", g.headers(secret), "file", a, &up); err != nil {
		return nil, err
	}
	link := up.FullPath
	if link == "" {
		if up.URL == "" {
			return nil, fmt.Errorf("gitlab upload of %s returned no URL", a.Name)
		}
		link = "/" + joinPath(g.ns(t, spec), spec.Name) + up.URL
	}
	if !strings.HasPrefix(link, "http://") && !strings.HasPrefix(link, "https://") {
		link = g.web(t) + "/" + strings.TrimLeft(link, "/")
	}
	var l struct {
		ID json.Number `json:"id"`
	}
	body := map[string]any{"name": a.Name, "url": link, "link_type": "other"}
	if _, err := doJSON(ctx, http.MethodPost, g.releaseURL(t, spec, rr.ID)+"/assets/links", g.headers(secret), body, &l); err != nil {
		return nil, err
	}
	return &RemoteAsset{ID: idString(l.ID), Name: a.Name, Size: a.Size}, nil
}

func (g *GitLab) DeleteAsset(ctx context.Context, t *store.Target, secret string, spec RepoSpec, rr *RemoteRelease, assetID string) error {
	_, err := doJSON(ctx, http.MethodDelete, g.releaseURL(t, spec, rr.ID)+"/assets/links/"+url.PathEscape(assetID), g.headers(secret), nil, nil)
	if statusOf(err) == http.StatusNotFound {
		return nil
	}
	return err
}
