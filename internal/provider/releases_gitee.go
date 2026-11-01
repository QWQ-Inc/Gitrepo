package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/qwq-inc/gitrepo/internal/store"
)

// Gitee releases: the access token travels as a query parameter (see call),
// attachments are "attach_files", and creating a release needs a target
// commit, a name and a description.

type giteeAttach struct {
	ID   json.Number `json:"id"`
	Name string      `json:"name"`
	Size int64       `json:"size"`
}

func (g *Gitee) withToken(u, secret string) string {
	return u + "?access_token=" + url.QueryEscape(secret)
}

func (g *Gitee) GetRelease(ctx context.Context, t *store.Target, secret string, spec RepoSpec, tag string) (*RemoteRelease, error) {
	var r struct {
		ID json.Number `json:"id"`
	}
	if _, err := g.call(ctx, http.MethodGet, g.repoAPI(t, spec)+"/releases/tags/"+url.PathEscape(tag), secret, nil, &r); err != nil {
		if statusOf(err) == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	if r.ID == "" || r.ID == "0" {
		return nil, nil // Gitee answers {} for a tag without a release
	}
	rr := &RemoteRelease{ID: idString(r.ID), Key: idString(r.ID)}
	// Attachments are paginated (at most 100 per page).
	for page := 1; page <= 100; page++ {
		var files []giteeAttach
		u := g.repoAPI(t, spec) + "/releases/" + url.PathEscape(rr.ID) + "/attach_files?per_page=100&page=" + strconv.Itoa(page)
		if _, err := g.call(ctx, http.MethodGet, u, secret, nil, &files); err != nil {
			return nil, err
		}
		for _, f := range files {
			rr.Assets = append(rr.Assets, RemoteAsset{ID: idString(f.ID), Name: f.Name, Size: f.Size})
		}
		if len(files) < 100 {
			break
		}
	}
	return rr, nil
}

func giteeReleaseBody(rel Release) map[string]any {
	name, body := rel.Name, rel.Body
	if name == "" {
		name = rel.Tag
	}
	if body == "" {
		body = name // required by Gitee
	}
	return map[string]any{"tag_name": rel.Tag, "name": name, "body": body, "prerelease": rel.Prerelease, "target_commitish": rel.Commit}
}

func (g *Gitee) CreateRelease(ctx context.Context, t *store.Target, secret string, spec RepoSpec, rel Release) (*RemoteRelease, error) {
	var r struct {
		ID json.Number `json:"id"`
	}
	if _, err := g.call(ctx, http.MethodPost, g.repoAPI(t, spec)+"/releases", secret, giteeReleaseBody(rel), &r); err != nil {
		return nil, err
	}
	return &RemoteRelease{ID: idString(r.ID), Key: idString(r.ID)}, nil
}

func (g *Gitee) UpdateRelease(ctx context.Context, t *store.Target, secret string, spec RepoSpec, rr *RemoteRelease, rel Release) error {
	body := giteeReleaseBody(rel)
	delete(body, "target_commitish")
	_, err := g.call(ctx, http.MethodPatch, g.repoAPI(t, spec)+"/releases/"+url.PathEscape(rr.ID), secret, body, nil)
	return err
}

func (g *Gitee) DeleteRelease(ctx context.Context, t *store.Target, secret string, spec RepoSpec, rr *RemoteRelease) error {
	_, err := g.call(ctx, http.MethodDelete, g.repoAPI(t, spec)+"/releases/"+url.PathEscape(rr.ID), secret, nil, nil)
	if statusOf(err) == http.StatusNotFound {
		return nil
	}
	return err
}

func (g *Gitee) UploadAsset(ctx context.Context, t *store.Target, secret string, spec RepoSpec, rr *RemoteRelease, a Asset) (*RemoteAsset, error) {
	var out giteeAttach
	u := g.withToken(g.repoAPI(t, spec)+"/releases/"+url.PathEscape(rr.ID)+"/attach_files", secret)
	if err := uploadMultipart(ctx, u, nil, "file", a, &out); err != nil {
		return nil, scrub(err, secret)
	}
	return &RemoteAsset{ID: idString(out.ID), Name: out.Name, Size: out.Size}, nil
}

func (g *Gitee) DeleteAsset(ctx context.Context, t *store.Target, secret string, spec RepoSpec, rr *RemoteRelease, assetID string) error {
	_, err := g.call(ctx, http.MethodDelete, g.repoAPI(t, spec)+"/releases/"+url.PathEscape(rr.ID)+"/attach_files/"+url.PathEscape(assetID), secret, nil, nil)
	if statusOf(err) == http.StatusNotFound {
		return nil
	}
	return err
}
