// Package notify delivers Gitrepo events to outbound webhooks: generic
// JSON (HMAC-signed) for downstream automation, or text messages for Slack,
// WeCom (企业微信), Feishu (飞书) and DingTalk (钉钉) bots.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qwq-inc/gitrepo/internal/auth"
	"github.com/qwq-inc/gitrepo/internal/secrets"
	"github.com/qwq-inc/gitrepo/internal/store"
)

// Event types.
const (
	EventPush               = "push"
	EventSyncCompleted      = "sync.completed"
	EventRemoteDiverged     = "remote.diverged"
	EventRemoteFailed       = "remote.failed"
	EventRemoteRecovered    = "remote.recovered"
	EventCredentialExpiring = "credential.expiring"
	EventPing               = "ping"
)

// AllEvents lists the event types a webhook can subscribe to.
var AllEvents = []string{EventPush, EventSyncCompleted, EventRemoteDiverged, EventRemoteFailed, EventRemoteRecovered, EventCredentialExpiring}

// Formats.
var Formats = []string{"json", "slack", "wecom", "feishu", "dingtalk"}

// RemoteSummary is one mirror's result inside sync.completed.
type RemoteSummary struct {
	Remote    string   `json:"remote"`
	State     string   `json:"state"`
	Changed   int      `json:"changed"`
	Error     string   `json:"error,omitempty"`
	Divergent []string `json:"divergent,omitempty"`
}

// Event is the JSON payload of every webhook delivery.
type Event struct {
	Event    string    `json:"event"`
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	Repo     string    `json:"repo,omitempty"`
	CloneURL string    `json:"clone_url,omitempty"`
	// Refs: for push, the new value of every pushed ref ("" = deleted); for
	// sync.completed, refs updated on at least one mirror.
	Refs      map[string]string `json:"refs,omitempty"`
	Pusher    string            `json:"pusher,omitempty"`
	Trigger   string            `json:"trigger,omitempty"`
	Remotes   []RemoteSummary   `json:"remotes,omitempty"`
	Remote    string            `json:"remote,omitempty"`
	State     string            `json:"state,omitempty"`
	Error     string            `json:"error,omitempty"`
	Divergent []string          `json:"divergent,omitempty"`
	Failures  int               `json:"consecutive_failures,omitempty"`
	Target    string            `json:"target,omitempty"`
	ExpiresAt *time.Time        `json:"expires_at,omitempty"`
}

// Text renders a one-paragraph human summary for chat formats.
func (e *Event) Text() string {
	var b strings.Builder
	switch e.Event {
	case EventPush:
		fmt.Fprintf(&b, "[Gitrepo] %s received a push", e.Repo)
		if e.Pusher != "" {
			fmt.Fprintf(&b, " from %s", e.Pusher)
		}
		writeRefs(&b, e.Refs)
	case EventSyncCompleted:
		fmt.Fprintf(&b, "[Gitrepo] %s synced to mirrors (%s)", e.Repo, e.Trigger)
		for _, r := range e.Remotes {
			fmt.Fprintf(&b, "\n- %s: %s", r.Remote, r.State)
			if r.Changed > 0 {
				fmt.Fprintf(&b, ", %d ref(s) updated", r.Changed)
			}
			if r.Error != "" {
				fmt.Fprintf(&b, ", %s", r.Error)
			}
		}
	case EventRemoteDiverged:
		fmt.Fprintf(&b, "[Gitrepo] mirror %s of %s has diverged: %s\nSomeone changed the mirror directly. Merge its commits and push to the hub, or run: gr sync %s --overwrite",
			e.Remote, e.Repo, strings.Join(e.Divergent, ", "), e.Repo)
	case EventRemoteFailed:
		fmt.Fprintf(&b, "[Gitrepo] mirror %s of %s failed %d times in a row: %s", e.Remote, e.Repo, e.Failures, e.Error)
	case EventRemoteRecovered:
		fmt.Fprintf(&b, "[Gitrepo] mirror %s of %s is in sync again", e.Remote, e.Repo)
	case EventCredentialExpiring:
		when := "soon"
		if e.ExpiresAt != nil {
			when = "on " + e.ExpiresAt.Format("2006-01-02")
		}
		fmt.Fprintf(&b, "[Gitrepo] the credential of target %s expires %s; rotate it with: gr target update %s --token-env VAR", e.Target, when, e.Target)
	case EventPing:
		b.WriteString("[Gitrepo] webhook test")
	default:
		fmt.Fprintf(&b, "[Gitrepo] %s %s", e.Event, e.Repo)
	}
	return b.String()
}

func writeRefs(b *strings.Builder, refs map[string]string) {
	names := make([]string, 0, len(refs))
	for r := range refs {
		names = append(names, r)
	}
	sort.Strings(names)
	for i, r := range names {
		if i == 10 {
			fmt.Fprintf(b, "\n… and %d more", len(names)-10)
			break
		}
		sha := refs[r]
		if sha == "" {
			fmt.Fprintf(b, "\n- %s deleted", r)
		} else {
			fmt.Fprintf(b, "\n- %s → %.10s", r, sha)
		}
	}
}

// Body renders the request body for a webhook format.
func Body(format string, e *Event) ([]byte, error) {
	switch format {
	case "", "json":
		return json.Marshal(e)
	case "slack":
		return json.Marshal(map[string]any{"text": e.Text()})
	case "wecom", "dingtalk":
		return json.Marshal(map[string]any{"msgtype": "text", "text": map[string]string{"content": e.Text()}})
	case "feishu":
		return json.Marshal(map[string]any{"msg_type": "text", "content": map[string]string{"text": e.Text()}})
	default:
		return nil, fmt.Errorf("unknown webhook format %q", format)
	}
}

// Sign returns the X-Gitrepo-Signature-256 header value for body.
func Sign(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// Matches reports whether a subscription list covers event ("*", exact, or
// a "remote.*" prefix).
func Matches(subs []string, event string) bool {
	if len(subs) == 0 {
		return true
	}
	for _, s := range subs {
		if s == "*" || s == event || (strings.HasSuffix(s, ".*") && strings.HasPrefix(event, strings.TrimSuffix(s, "*"))) {
			return true
		}
	}
	return false
}

// ----------------------------------------------------------------- notifier

type job struct {
	hook  *store.Webhook
	event *Event
}

// Notifier fans events out to matching webhooks with retries.
type Notifier struct {
	store  *store.Store
	box    *secrets.Box
	log    *slog.Logger
	client *http.Client
	// Backoff between delivery attempts; the first attempt is immediate.
	Backoff []time.Duration

	queue  chan job
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func New(st *store.Store, box *secrets.Box, log *slog.Logger) *Notifier {
	ctx, cancel := context.WithCancel(context.Background())
	return &Notifier{
		store: st, box: box, log: log,
		client:  &http.Client{Timeout: 15 * time.Second},
		Backoff: []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute},
		queue:   make(chan job, 1000),
		ctx:     ctx, cancel: cancel,
	}
}

// Start launches the delivery workers.
func (n *Notifier) Start(workers int) {
	for i := 0; i < workers; i++ {
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			for {
				select {
				case <-n.ctx.Done():
					return
				case j := <-n.queue:
					n.deliverWithRetry(j)
				}
			}
		}()
	}
}

// Stop abandons pending deliveries and waits for workers.
func (n *Notifier) Stop() {
	n.cancel()
	n.wg.Wait()
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Emit queues e for every enabled webhook subscribed to it. It never blocks.
func (n *Notifier) Emit(e Event) {
	if n == nil {
		return
	}
	if e.ID == "" {
		e.ID = newID()
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	hooks, err := n.store.ListWebhooks(n.ctx)
	if err != nil {
		n.log.Error("load webhooks", "err", err)
		return
	}
	for _, h := range hooks {
		if !h.Enabled || !Matches(h.Events, e.Event) {
			continue
		}
		if !wants(h.Repos, e.Repo) {
			continue
		}
		ev := e
		select {
		case n.queue <- job{hook: h, event: &ev}:
		default:
			n.log.Warn("webhook queue full; dropping event", "webhook", h.Name, "event", e.Event)
		}
	}
}

// wants reports whether a webhook limited to the repository patterns gets
// an event about repo. Instance-wide events (no repository, such as
// credential.expiring) go only to webhooks without a repository filter, or
// one that matches every repository: a webhook set up for one team's
// repositories must not learn about the whole instance's credentials.
func wants(patterns []string, repo string) bool {
	if repo != "" {
		return auth.MatchRepo(patterns, repo)
	}
	for _, p := range patterns {
		if p != "*" && p != "*/*" {
			return false
		}
	}
	return true
}

// Test delivers a ping synchronously and returns the outcome.
func (n *Notifier) Test(ctx context.Context, h *store.Webhook) error {
	e := &Event{Event: EventPing, ID: newID(), Time: time.Now().UTC()}
	status, err := n.deliver(ctx, h, e)
	_ = n.store.RecordWebhookDelivery(context.Background(), h.ID, status)
	return err
}

func (n *Notifier) deliverWithRetry(j job) {
	var status string
	var err error
	for attempt := 0; ; attempt++ {
		status, err = n.deliver(n.ctx, j.hook, j.event)
		if err == nil || attempt >= len(n.Backoff) {
			break
		}
		select {
		case <-n.ctx.Done():
			return
		case <-time.After(n.Backoff[attempt]):
		}
	}
	if err != nil {
		n.log.Warn("webhook delivery failed", "webhook", j.hook.Name, "event", j.event.Event, "err", err)
	}
	_ = n.store.RecordWebhookDelivery(context.Background(), j.hook.ID, status)
}

// deliver performs one POST and returns a short status for display.
func (n *Notifier) deliver(ctx context.Context, h *store.Webhook, e *Event) (string, error) {
	rawURL, err := n.box.Open(h.URL, h.WebhookAAD("url"))
	if err != nil {
		return "error: " + err.Error(), err
	}
	body, err := Body(h.Format, e)
	if err != nil {
		return "error: " + err.Error(), err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, string(rawURL), bytes.NewReader(body))
	if err != nil {
		return "error: invalid url", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "gitrepo-webhook")
	req.Header.Set("X-Gitrepo-Event", e.Event)
	req.Header.Set("X-Gitrepo-Delivery", e.ID)
	if len(h.Secret) > 0 {
		secret, err := n.box.Open(h.Secret, h.WebhookAAD("secret"))
		if err != nil {
			return "error: " + err.Error(), err
		}
		req.Header.Set("X-Gitrepo-Signature-256", Sign(secret, body))
	}
	resp, err := n.client.Do(req)
	if err != nil {
		// Do not echo the URL: chat-bot URLs embed their access key.
		msg := fmt.Sprintf("error: request to %s failed", h.URLHost)
		return msg, fmt.Errorf("%s", msg)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	status := resp.Status
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return status, fmt.Errorf("webhook %s returned %s", h.Name, status)
	}
	return status, nil
}
