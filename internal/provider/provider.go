// Package provider adapts hosting platforms. A provider knows how to build a
// remote's Git URL and credential and, when the platform has an API, how to
// create repositories and relax branch protection so mirrors can be
// force-updated. Adding a platform means implementing Provider.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/qwq-inc/gitrepo/internal/gitexec"
	"github.com/qwq-inc/gitrepo/internal/store"
)

// Caps advertises optional platform features.
type Caps struct {
	// CreateRepo: EnsureRepo can create missing repositories.
	CreateRepo bool
	// BranchProtection: AllowForce can lift protection that blocks mirror
	// force-pushes and deletions.
	BranchProtection bool
	// DefaultBranch: SetDefaultBranch is supported.
	DefaultBranch bool
}

// RepoSpec identifies a repository on a target.
type RepoSpec struct {
	Namespace  string
	Name       string
	Visibility string // private | public | internal
}

// Provider is implemented by each platform adapter.
type Provider interface {
	Kind() string
	Caps() Caps
	// Validate checks target configuration before it is stored.
	Validate(t *store.Target) error
	// RepoURL returns the Git URL for a repository on the target.
	RepoURL(t *store.Target, namespace, name string) (string, error)
	// GitCred builds the git credential from the decrypted secret.
	GitCred(t *store.Target, secret string) *gitexec.Cred
	// MaxFileSize is the platform's per-file limit (0 = none known).
	MaxFileSize() int64
	// EnsureRepo creates the repository if missing. created reports whether
	// it was created by this call.
	EnsureRepo(ctx context.Context, t *store.Target, secret string, spec RepoSpec) (created bool, err error)
	// AllowForce lifts protection so the listed branches can be force-updated
	// and the listed deletions performed.
	AllowForce(ctx context.Context, t *store.Target, secret string, spec RepoSpec, force, del []string) error
	// SetDefaultBranch sets the repository's default branch.
	SetDefaultBranch(ctx context.Context, t *store.Target, secret string, spec RepoSpec, branch string) error
	// Check verifies the credential and returns the authenticated identity.
	Check(ctx context.Context, t *store.Target, secret string) (string, error)
}

// LoginUsername is implemented by providers whose Git-over-HTTPS username
// must be the token owner's login. When a target has no username, the API
// fills it in from Check.
type LoginUsername interface {
	UsesLoginUsername() bool
}

var registry = map[string]Provider{}

func register(p Provider) { registry[p.Kind()] = p }

// Get returns the provider for kind.
func Get(kind string) (Provider, error) {
	p, ok := registry[kind]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q (supported: %s)", kind, strings.Join(Kinds(), ", "))
	}
	return p, nil
}

// Kinds lists registered provider kinds.
func Kinds() []string {
	return []string{"github", "gitlab", "gitea", "gitee", "generic"}
}

func init() {
	register(&GitHub{})
	register(&GitLab{})
	register(&Gitea{})
	register(&Gitee{})
	register(&Generic{})
}

// ---------------------------------------------------------------- helpers

var httpClient = &http.Client{Timeout: 30 * time.Second}

// APIError is a non-2xx platform API response.
type APIError struct {
	Method string
	URL    string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 500 {
		body = body[:500] + "…"
	}
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, redactURL(e.URL), e.Status, body)
}

func redactURL(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		// Unparsable: drop everything that could hold a credential.
		if i := strings.IndexAny(u, "?#"); i >= 0 {
			u = u[:i]
		}
		if i := strings.Index(u, "@"); i >= 0 {
			if j := strings.Index(u, "://"); j >= 0 && j < i {
				u = u[:j+3] + u[i+1:]
			}
		}
		return u
	}
	p.User = nil
	p.RawQuery = ""
	return p.String()
}

// doJSON performs an API call. in (if non-nil) is JSON-encoded; out (if
// non-nil) receives the decoded 2xx response body.
func doJSON(ctx context.Context, method, u string, headers map[string]string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return 0, fmt.Errorf("%s %s: invalid request URL", method, redactURL(u))
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "gitrepo")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// Transport errors embed the full URL, which may carry a token.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, fmt.Errorf("%s %s: %w", method, redactURL(u), err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, &APIError{Method: method, URL: u, Status: resp.StatusCode, Body: string(raw)}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: decode response: %w", method, redactURL(u), err)
		}
	}
	return resp.StatusCode, nil
}

func statusOf(err error) int {
	if e, ok := err.(*APIError); ok {
		return e.Status
	}
	return 0
}

func joinPath(parts ...string) string {
	var out []string
	for _, p := range parts {
		p = strings.Trim(p, "/")
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "/")
}
