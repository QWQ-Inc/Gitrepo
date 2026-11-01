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

type ghRelease struct {
	ID        json.Number `json:"id"`
	UploadURL string      `json:"upload_url"`
	Assets    []struct {
		ID   json.Number `json:"id"`
		Name string      `json:"name"`
		Size int64       `json:"size"`
	} `json:"assets"`
}

func (r *ghRelease) remote() *RemoteRelease {
	rr := &RemoteRelease{ID: idString(r.ID), Key: idString(r.ID), uploadURL: r.UploadURL}
	for _, a := range r.Assets {
		rr.Assets = append(rr.Assets, RemoteAsset{ID: idString(a.ID), Name: a.Name, Size: a.Size})
	}
	return rr
}

func (g *GitHub) releasesAPI(t *store.Target, spec RepoSpec) string {
	return g.api(t) + "/repos/" + url.PathEscape(g.ns(t, spec)) + "/" + url.PathEscape(spec.Name) + "/releases"
}

func (g *GitHub) GetRelease(ctx context.Context, t *store.Target, secret string, spec RepoSpec, tag string) (*RemoteRelease, error) {
	var r ghRelease
	if _, err := doJSON(ctx, http.MethodGet, g.releasesAPI(t, spec)+"/tags/"+escapeTag(tag), g.headers(secret), nil, &r); err != nil {
		if statusOf(err) == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	return r.remote(), nil
}

func (g *GitHub) CreateRelease(ctx context.Context, t *store.Target, secret string, spec RepoSpec, rel Release) (*RemoteRelease, error) {
	var r ghRelease
	// "legacy": the latest release is the highest version, so publishing a
	// backport does not make it "Latest". The target only matters if the
	// tag vanished from the mirror; then GitHub recreates it at our commit.
	body := map[string]any{"tag_name": rel.Tag, "name": rel.Name, "body": rel.Body, "prerelease": rel.Prerelease, "draft": false, "make_latest": "legacy"}
	if rel.Commit != "" {
		body["target_commitish"] = rel.Commit
	}
	if _, err := doJSON(ctx, http.MethodPost, g.releasesAPI(t, spec), g.headers(secret), body, &r); err != nil {
		return nil, err
	}
	return r.remote(), nil
}

func (g *GitHub) UpdateRelease(ctx context.Context, t *store.Target, secret string, spec RepoSpec, rr *RemoteRelease, rel Release) error {
	body := map[string]any{"name": rel.Name, "body": rel.Body, "prerelease": rel.Prerelease, "make_latest": "legacy"}
	_, err := doJSON(ctx, http.MethodPatch, g.releasesAPI(t, spec)+"/"+url.PathEscape(rr.ID), g.headers(secret), body, nil)
	return err
}

func (g *GitHub) DeleteRelease(ctx context.Context, t *store.Target, secret string, spec RepoSpec, rr *RemoteRelease) error {
	_, err := doJSON(ctx, http.MethodDelete, g.releasesAPI(t, spec)+"/"+url.PathEscape(rr.ID), g.headers(secret), nil, nil)
	if statusOf(err) == http.StatusNotFound {
		return nil
	}
	return err
}

func (g *GitHub) UploadAsset(ctx context.Context, t *store.Target, secret string, spec RepoSpec, rr *RemoteRelease, a Asset) (*RemoteAsset, error) {
	target, err := g.uploadTarget(t, rr.uploadURL)
	if err != nil {
		return nil, err
	}
	var out struct {
		ID   json.Number `json:"id"`
		Name string      `json:"name"`
		Size int64       `json:"size"`
	}
	if err := uploadRaw(ctx, target+"?name="+url.QueryEscape(a.Name), g.headers(secret), a, &out); err != nil {
		return nil, err
	}
	return &RemoteAsset{ID: idString(out.ID), Name: out.Name, Size: out.Size}, nil
}

// uploadTarget checks GitHub's upload_url before the token is sent to it:
// it must be the API host, or uploads.github.com for github.com.
func (g *GitHub) uploadTarget(t *store.Target, raw string) (string, error) {
	if i := strings.Index(raw, "{"); i >= 0 {
		raw = raw[:i]
	}
	u, err := url.Parse(raw)
	api, aerr := url.Parse(g.api(t))
	if err != nil || aerr != nil || raw == "" {
		return "", fmt.Errorf("github returned an unusable upload URL")
	}
	ok := u.Scheme == api.Scheme && strings.EqualFold(u.Host, api.Host)
	if strings.EqualFold(api.Host, "api.github.com") && u.Scheme == "https" && strings.EqualFold(u.Host, "uploads.github.com") {
		ok = true
	}
	if !ok {
		return "", fmt.Errorf("github returned an upload URL on an unexpected host (%s)", u.Host)
	}
	return u.String(), nil
}

func (g *GitHub) DeleteAsset(ctx context.Context, t *store.Target, secret string, spec RepoSpec, rr *RemoteRelease, assetID string) error {
	_, err := doJSON(ctx, http.MethodDelete, g.releasesAPI(t, spec)+"/assets/"+url.PathEscape(assetID), g.headers(secret), nil, nil)
	if statusOf(err) == http.StatusNotFound {
		return nil
	}
	return err
}
