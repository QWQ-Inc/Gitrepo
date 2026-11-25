// Package deploy tells deployment platforms (StackEnigma, or anything that
// takes a webhook) that a branch has a new commit.
//
// A deploy hook watches one branch of one repository, either on the hub
// (it fires as soon as a push lands) or on one mirror (it fires once a sync
// has put the commit there: a platform that builds from GitHub must not be
// told before GitHub has the commit). Deliveries are signed per Standard
// Webhooks (webhook-id, webhook-timestamp, webhook-signature) and, for
// receivers that check a Gitrepo-style signature, X-Gitrepo-Signature-256;
// a bearer token can be sent too. Deliveries of one hook are serialized
// and coalesced: while one is in flight only the newest commit waits, which
// is what a deployment wants.
package deploy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qwq-inc/gitrepo/internal/secrets"
	"github.com/qwq-inc/gitrepo/internal/store"
	"github.com/qwq-inc/gitrepo/internal/syncer"
)

// Kinds. A kind is a preset: it only changes the defaults and labels.
const (
	KindGeneric     = "generic"
	KindStackEnigma = "stackenigma"
)

// Kinds lists the valid kinds.
var Kinds = []string{KindGeneric, KindStackEnigma}

// Triggers.
const (
	TriggerPush   = "push"
	TriggerSync   = "sync"
	TriggerManual = "manual"
)

// EventType is the payload's type.
const EventType = "deploy.requested"

// Payload is the JSON body of a delivery (Standard Webhooks shape).
type Payload struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Data      Data      `json:"data"`
}

// Data says what to deploy.
type Data struct {
	Hook       string `json:"hook"`
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	Ref        string `json:"ref"`
	Commit     string `json:"commit"`
	// Source is "hub" or the mirror ("github:acme/app") the commit is on.
	Source    string `json:"source"`
	SourceURL string `json:"source_url"`
	CloneURL  string `json:"clone_url"`
	Trigger   string `json:"trigger"`
	Pusher    string `json:"pusher,omitempty"`
}

// SecretKey returns the signing key for a secret: a Standard Webhooks
// secret ("whsec_" + base64) is decoded; anything else is used as is.
func SecretKey(secret string) ([]byte, error) {
	if rest, ok := strings.CutPrefix(secret, "whsec_"); ok {
		b, err := base64.StdEncoding.DecodeString(rest)
		if err != nil || len(b) == 0 {
			return nil, errors.New(`a "whsec_" secret must be followed by base64`)
		}
		return b, nil
	}
	return []byte(secret), nil
}

// Sign returns the Standard Webhooks signature header value.
func Sign(key []byte, id string, ts int64, body []byte) string {
	m := hmac.New(sha256.New, key)
	fmt.Fprintf(m, "%s.%d.", id, ts)
	m.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// Verify checks a Standard Webhooks signature header (several
// space-separated signatures may be present).
func Verify(key []byte, id string, ts int64, body []byte, header string) bool {
	want := Sign(key, id, ts, body)
	for _, s := range strings.Fields(header) {
		if hmac.Equal([]byte(s), []byte(want)) {
			return true
		}
	}
	return false
}

// CheckURL validates a hook URL.
func CheckURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("url must be an http(s) URL")
	}
	if u.User != nil {
		return nil, errors.New("put credentials in the token, not in the URL")
	}
	return u, nil
}

// Resolver returns the current value of ref on the hub (remoteID 0) or on
// a mirror ("" when it has none).
type Resolver func(ctx context.Context, repo *store.Repo, remoteID int64, ref string) (string, error)

// Manager delivers deploy hooks.
type Manager struct {
	Store     *store.Store
	Box       *secrets.Box
	Log       *slog.Logger
	PublicURL string
	Client    *http.Client
	Resolve   Resolver
	// Backoff between delivery attempts; the first attempt is immediate.
	Backoff []time.Duration

	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	mu      sync.Mutex
	running map[int64]bool
	pending map[int64]*job
}

type job struct {
	hook    *store.DeployHook
	payload Payload
}

// New returns a Manager; call Stop when done.
func New(st *store.Store, box *secrets.Box, log *slog.Logger, publicURL string) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		Store: st, Box: box, Log: log, PublicURL: strings.TrimRight(publicURL, "/"),
		Client:  &http.Client{Timeout: 30 * time.Second},
		Backoff: []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute},
		ctx:     ctx, cancel: cancel,
		running: map[int64]bool{}, pending: map[int64]*job{},
	}
}

// Stop abandons pending deliveries and waits for running ones.
func (m *Manager) Stop() {
	m.cancel()
	m.wg.Wait()
}

// Branch returns the branch a hook watches, as named on its source: the
// configured one, or the repository's default branch (under the name the
// mirror's view gives it).
func Branch(h *store.DeployHook, repo *store.Repo, rem *store.Remote) string {
	if h.Branch != "" {
		return h.Branch
	}
	def := repo.DefaultBranch
	if def == "" {
		def = "main"
	}
	if rem == nil {
		return def
	}
	v, err := syncer.ViewOf(rem)
	if err != nil {
		return def
	}
	if dst, ok := v.Map("refs/heads/" + def); ok {
		return strings.TrimPrefix(dst, "refs/heads/")
	}
	return def
}

// HubUpdated is called after a push to the hub (refs: ref → new value,
// "" = deleted).
func (m *Manager) HubUpdated(repo *store.Repo, refs map[string]string, pusher string) {
	m.updated(repo, nil, refs, TriggerPush, pusher)
}

// MirrorUpdated is called after a sync put refs on a mirror (mirror ref
// names → values).
func (m *Manager) MirrorUpdated(repo *store.Repo, rem *store.Remote, refs map[string]string) {
	m.updated(repo, rem, refs, TriggerSync, "")
}

func (m *Manager) updated(repo *store.Repo, rem *store.Remote, refs map[string]string, trigger, pusher string) {
	if m == nil || len(refs) == 0 {
		return
	}
	hooks, err := m.Store.ListDeployHooks(m.ctx, repo.ID)
	if err != nil {
		m.Log.Error("deploy hooks: load", "repo", repo.Slug, "err", err)
		return
	}
	var remoteID int64
	if rem != nil {
		remoteID = rem.ID
	}
	for _, h := range hooks {
		if !h.Enabled || h.RemoteID != remoteID {
			continue
		}
		branch := Branch(h, repo, rem)
		sha := refs["refs/heads/"+branch]
		if sha == "" || sha == h.LastCommit {
			continue
		}
		m.enqueue(&job{hook: h, payload: m.payload(h, repo, rem, branch, sha, trigger, pusher)})
	}
}

func (m *Manager) payload(h *store.DeployHook, repo *store.Repo, rem *store.Remote, branch, sha, trigger, pusher string) Payload {
	d := Data{
		Hook: h.Name, Repository: repo.Slug, Branch: branch, Ref: "refs/heads/" + branch, Commit: sha,
		Source: "hub", CloneURL: m.PublicURL + "/" + repo.Slug + ".git", Trigger: trigger, Pusher: pusher,
	}
	d.SourceURL = d.CloneURL
	if rem != nil {
		d.Source, d.SourceURL = rem.Label(), rem.URL
	}
	return Payload{Type: EventType, Timestamp: time.Now().UTC(), Data: d}
}

func (m *Manager) enqueue(j *job) {
	id := j.hook.ID
	m.mu.Lock()
	if m.running[id] {
		m.pending[id] = j // only the newest commit is worth deploying
		m.mu.Unlock()
		return
	}
	m.running[id] = true
	m.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for j != nil {
			m.deliverWithRetry(j)
			m.mu.Lock()
			j = m.pending[id]
			delete(m.pending, id)
			if j == nil {
				delete(m.running, id)
			}
			m.mu.Unlock()
		}
	}()
}

func (m *Manager) superseded(id int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pending[id] != nil
}

func (m *Manager) deliverWithRetry(j *job) {
	for attempt := 0; ; attempt++ {
		status, err := m.deliver(m.ctx, j.hook, j.payload)
		m.record(j.hook, status, j.payload.Data.Commit, err == nil)
		if err == nil {
			m.Log.Info("deploy hook delivered", "hook", j.hook.Name, "commit", j.payload.Data.Commit, "status", status)
			return
		}
		if attempt >= len(m.Backoff) || m.superseded(j.hook.ID) {
			m.Log.Warn("deploy hook failed", "hook", j.hook.Name, "commit", j.payload.Data.Commit, "err", err)
			return
		}
		select {
		case <-m.ctx.Done():
			return
		case <-time.After(m.Backoff[attempt]):
		}
	}
}

func (m *Manager) record(h *store.DeployHook, status, commit string, ok bool) {
	if err := m.Store.RecordDeployDelivery(context.Background(), h.ID, status, commit, ok); err != nil {
		m.Log.Warn("deploy hook: record delivery", "hook", h.Name, "err", err)
	}
}

func newID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return "dep_" + hex.EncodeToString(b)
}

// deliver performs one POST and returns a short status for display.
func (m *Manager) deliver(ctx context.Context, h *store.DeployHook, p Payload) (string, error) {
	rawURL, err := m.Box.Open(h.URL, h.AAD("url"))
	if err != nil {
		return "error: " + err.Error(), err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return "error: " + err.Error(), err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, string(rawURL), bytes.NewReader(body))
	if err != nil {
		return "error: invalid url", err
	}
	id, ts := newID(), p.Timestamp.Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "gitrepo-deploy")
	req.Header.Set("X-Gitrepo-Event", "deploy")
	req.Header.Set("X-Gitrepo-Delivery", id)
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", strconv.FormatInt(ts, 10))
	if len(h.Secret) > 0 {
		secret, err := m.Box.Open(h.Secret, h.AAD("secret"))
		if err != nil {
			return "error: " + err.Error(), err
		}
		key, err := SecretKey(string(secret))
		if err != nil {
			return "error: " + err.Error(), err
		}
		req.Header.Set("webhook-signature", Sign(key, id, ts, body))
		mac := hmac.New(sha256.New, key)
		mac.Write(body)
		req.Header.Set("X-Gitrepo-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	if len(h.Token) > 0 {
		tok, err := m.Box.Open(h.Token, h.AAD("token"))
		if err != nil {
			return "error: " + err.Error(), err
		}
		req.Header.Set("Authorization", "Bearer "+string(tok))
	}
	resp, err := m.Client.Do(req)
	if err != nil {
		// Do not echo the URL: it may embed a token.
		msg := fmt.Sprintf("error: request to %s failed", h.URLHost)
		return msg, errors.New(msg)
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	status := resp.Status
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if s := strings.TrimSpace(string(snippet)); s != "" && !strings.HasPrefix(s, "<") {
			status += ": " + strings.Join(strings.Fields(s), " ")
		}
		return status, fmt.Errorf("deploy hook %s returned %s", h.Name, resp.Status)
	}
	return status, nil
}

// Trigger delivers the branch's current commit now (synchronously, one
// attempt) and returns the commit and the outcome.
func (m *Manager) Trigger(ctx context.Context, repo *store.Repo, h *store.DeployHook) (commit, status string, err error) {
	var rem *store.Remote
	if h.RemoteID != 0 {
		if rem, err = m.Store.GetRemote(ctx, h.RemoteID); err != nil {
			return "", "", err
		}
	}
	branch := Branch(h, repo, rem)
	if m.Resolve == nil {
		return "", "", errors.New("no resolver")
	}
	sha, err := m.Resolve(ctx, repo, h.RemoteID, "refs/heads/"+branch)
	if err != nil {
		return "", "", err
	}
	if sha == "" {
		where := "the hub"
		if rem != nil {
			where = rem.Label() + " (as far as Gitrepo has pushed)"
		}
		return "", "", fmt.Errorf("branch %s does not exist on %s", branch, where)
	}
	p := m.payload(h, repo, rem, branch, sha, TriggerManual, "")
	status, err = m.deliver(ctx, h, p)
	m.record(h, status, sha, err == nil)
	return sha, status, err
}
