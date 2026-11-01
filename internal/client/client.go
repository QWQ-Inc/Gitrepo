// Package client is a small Go client for the Gitrepo admin API.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{Timeout: 15 * time.Minute}}
}

// Error is a non-2xx API response.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("%s (HTTP %d)", e.Message, e.Status) }

// Do sends a JSON request and decodes the JSON response into out.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.send(c.HTTP, req, out)
}

// Upload sends body (size bytes) as the raw request body, e.g. a release
// file, without the client's overall timeout; ctx bounds it.
func (c *Client) Upload(ctx context.Context, method, path, contentType string, body io.Reader, size int64, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", contentType)
	hc := *c.HTTP
	hc.Timeout = 0
	return c.send(&hc, req, out)
}

// Download streams a GET response body into w.
func (c *Client) Download(ctx context.Context, path string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	hc := *c.HTTP
	hc.Timeout = 0
	return c.send(&hc, req, w)
}

func (c *Client) send(hc *http.Client, req *http.Request, out any) error {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if w, ok := out.(io.Writer); ok && resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		_, err := io.Copy(w, resp.Body)
		return err
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		if msg == "" {
			msg = resp.Status
		}
		return &Error{Status: resp.StatusCode, Message: msg}
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// ---------------------------------------------------------------- types

type Target struct {
	ID           int64      `json:"id"`
	Name         string     `json:"name"`
	Provider     string     `json:"provider"`
	BaseURL      string     `json:"base_url,omitempty"`
	APIURL       string     `json:"api_url,omitempty"`
	Namespace    string     `json:"namespace,omitempty"`
	URLTemplate  string     `json:"url_template,omitempty"`
	Visibility   string     `json:"visibility,omitempty"`
	AutoCreate   bool       `json:"auto_create"`
	MaxFileSize  int64      `json:"max_file_size,omitempty"`
	CredKind     string     `json:"credential_kind"`
	CredUsername string     `json:"credential_username,omitempty"`
	HasSecret    bool       `json:"has_secret"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
}

type Binding struct {
	ID           int64  `json:"id"`
	Org          string `json:"org"`
	Target       string `json:"target"`
	Namespace    string `json:"namespace,omitempty"`
	OnDivergence string `json:"on_divergence"`
	NameTemplate string `json:"name_template"`
}

type Divergence struct {
	Ref         string     `json:"ref"`
	HubSHA      string     `json:"hub_sha,omitempty"`
	RemoteSHA   string     `json:"remote_sha,omitempty"`
	ExpectedSHA string     `json:"expected_sha,omitempty"`
	Reason      string     `json:"reason"`
	DetectedAt  *time.Time `json:"detected_at"`
}

type Remote struct {
	ID                  int64        `json:"id"`
	Label               string       `json:"label"`
	Target              string       `json:"target"`
	Provider            string       `json:"provider"`
	Namespace           string       `json:"namespace,omitempty"`
	Name                string       `json:"name"`
	URL                 string       `json:"url"`
	OnDivergence        string       `json:"on_divergence"`
	Enabled             bool         `json:"enabled"`
	Adopt               bool         `json:"adopt"`
	NeedsAdoption       bool         `json:"needs_adoption,omitempty"`
	State               string       `json:"state"`
	LastError           string       `json:"last_error,omitempty"`
	ConsecutiveFailures int          `json:"consecutive_failures,omitempty"`
	LastSyncAt          *time.Time   `json:"last_sync_at,omitempty"`
	LastSuccessAt       *time.Time   `json:"last_success_at,omitempty"`
	NextRetryAt         *time.Time   `json:"next_retry_at,omitempty"`
	Divergences         []Divergence `json:"divergences,omitempty"`
}

type Repo struct {
	ID            int64      `json:"id"`
	Slug          string     `json:"slug"`
	CloneURL      string     `json:"clone_url"`
	SSHURL        string     `json:"ssh_url,omitempty"`
	DefaultBranch string     `json:"default_branch,omitempty"`
	Visibility    string     `json:"visibility"`
	State         string     `json:"state"`
	Busy          bool       `json:"busy"`
	CreatedAt     *time.Time `json:"created_at"`
	Remotes       []Remote   `json:"remotes"`
}

type Event struct {
	Type      string   `json:"type"`
	Total     int      `json:"total,omitempty"`
	Remote    string   `json:"remote,omitempty"`
	State     string   `json:"state,omitempty"`
	Changed   int      `json:"changed,omitempty"`
	Error     string   `json:"error,omitempty"`
	Divergent []string `json:"divergent,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

type SyncResult struct {
	Events  []Event `json:"events"`
	Timeout bool    `json:"timeout"`
	Queued  bool    `json:"queued"`
}

type Run struct {
	ID          int64      `json:"id"`
	Remote      string     `json:"remote"`
	Trigger     string     `json:"trigger"`
	Status      string     `json:"status"`
	RefsChanged int        `json:"refs_changed"`
	Error       string     `json:"error,omitempty"`
	Detail      string     `json:"detail,omitempty"`
	StartedAt   *time.Time `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

type Token struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	Repos      []string   `json:"repos,omitempty"`
	Token      string     `json:"token,omitempty"`
	CreatedAt  *time.Time `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

type Webhook struct {
	ID             int64      `json:"id"`
	Name           string     `json:"name"`
	Host           string     `json:"host"`
	Format         string     `json:"format"`
	Events         []string   `json:"events"`
	Repos          []string   `json:"repos,omitempty"`
	Signed         bool       `json:"signed"`
	Enabled        bool       `json:"enabled"`
	LastStatus     string     `json:"last_status,omitempty"`
	LastDeliveryAt *time.Time `json:"last_delivery_at,omitempty"`
}

type SSHKey struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Fingerprint string     `json:"fingerprint"`
	Type        string     `json:"type"`
	Scopes      []string   `json:"scopes"`
	Repos       []string   `json:"repos,omitempty"`
	CreatedAt   *time.Time `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
}

type SSHInfo struct {
	Enabled     bool   `json:"enabled"`
	Host        string `json:"host"`
	HostKey     string `json:"host_key"`
	Fingerprint string `json:"fingerprint"`
	ExampleURL  string `json:"example_url"`
}
