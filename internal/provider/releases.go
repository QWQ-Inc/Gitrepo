package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"github.com/qwq-inc/gitrepo/internal/store"
)

// Release is what Gitrepo publishes to a platform.
type Release struct {
	Tag        string
	Name       string
	Body       string
	Prerelease bool
	// Commit is the tagged commit (some platforms want a target).
	Commit string
}

// RemoteRelease is a release as it exists on a platform.
type RemoteRelease struct {
	// ID identifies the release in the platform's API (GitLab: the tag).
	ID     string
	Assets []RemoteAsset
	// uploadURL is GitHub's asset upload endpoint.
	uploadURL string
}

// RemoteAsset is a file attached to a platform release.
type RemoteAsset struct {
	ID   string
	Name string
	Size int64
}

// Asset is a file to attach.
type Asset struct {
	Name        string
	ContentType string
	Size        int64
	Open        func() (io.ReadCloser, error)
}

// Releaser is implemented by providers whose platform has releases.
type Releaser interface {
	// GetRelease returns the release for tag, or nil when there is none.
	GetRelease(ctx context.Context, t *store.Target, secret string, spec RepoSpec, tag string) (*RemoteRelease, error)
	CreateRelease(ctx context.Context, t *store.Target, secret string, spec RepoSpec, rel Release) (*RemoteRelease, error)
	UpdateRelease(ctx context.Context, t *store.Target, secret string, spec RepoSpec, rr *RemoteRelease, rel Release) error
	// DeleteRelease removes the release (the tag stays).
	DeleteRelease(ctx context.Context, t *store.Target, secret string, spec RepoSpec, rr *RemoteRelease) error
	UploadAsset(ctx context.Context, t *store.Target, secret string, spec RepoSpec, rr *RemoteRelease, a Asset) (*RemoteAsset, error)
	DeleteAsset(ctx context.Context, t *store.Target, secret string, spec RepoSpec, rr *RemoteRelease, assetID string) error
}

// escapeTag escapes a tag for a URL path, keeping "/" separators.
func escapeTag(tag string) string {
	parts := strings.Split(tag, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// uploadClient has no overall timeout: uploads can be large, and the
// caller's context bounds them.
var uploadClient = &http.Client{}

// doUpload sends a request with a streamed body of known size and decodes
// a JSON 2xx response into out.
func doUpload(ctx context.Context, method, u string, headers map[string]string, contentType string, body io.Reader, size int64, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return fmt.Errorf("%s %s: invalid request URL", method, redactURL(u))
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "gitrepo")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := uploadClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("%s %s: %w", method, redactURL(u), err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{Method: method, URL: u, Status: resp.StatusCode, Body: string(raw)}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%s %s: decode response: %w", method, redactURL(u), err)
		}
	}
	return nil
}

// uploadRaw sends the asset as the request body.
func uploadRaw(ctx context.Context, u string, headers map[string]string, a Asset, out any) error {
	f, err := a.Open()
	if err != nil {
		return err
	}
	defer f.Close()
	ct := a.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	return doUpload(ctx, http.MethodPost, u, headers, ct, f, a.Size, out)
}

// uploadMultipart sends the asset as a multipart/form-data file field,
// with an exact Content-Length (some servers refuse chunked uploads).
func uploadMultipart(ctx context.Context, u string, headers map[string]string, field string, a Asset, out any) error {
	f, err := a.Open()
	if err != nil {
		return err
	}
	defer f.Close()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if _, err := w.CreateFormFile(field, a.Name); err != nil {
		return err
	}
	headLen := buf.Len()
	if err := w.Close(); err != nil {
		return err
	}
	head := append([]byte(nil), buf.Bytes()[:headLen]...)
	tail := append([]byte(nil), buf.Bytes()[headLen:]...)
	body := io.MultiReader(bytes.NewReader(head), io.LimitReader(f, a.Size), bytes.NewReader(tail))
	return doUpload(ctx, http.MethodPost, u, headers, w.FormDataContentType(), body, int64(len(head))+a.Size+int64(len(tail)), out)
}

func idString(v json.Number) string { return v.String() }

var (
	_ Releaser = (*GitHub)(nil)
	_ Releaser = (*GitLab)(nil)
	_ Releaser = (*Gitea)(nil)
	_ Releaser = (*Gitee)(nil)
)
