// Package syncer pushes hub repositories to their remotes.
//
// Each repository has one serial queue: requests that arrive while a run is
// in progress are coalesced into the next run, so a burst of pushes produces
// at most one extra pass. A global semaphore bounds concurrent remote
// operations, and a lease row in the database guarantees one active run per
// repository even if two processes share a data directory by mistake.
package syncer

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/qwq-inc/gitrepo/internal/gitexec"
	"github.com/qwq-inc/gitrepo/internal/notify"
	"github.com/qwq-inc/gitrepo/internal/registry"
	"github.com/qwq-inc/gitrepo/internal/secrets"
	"github.com/qwq-inc/gitrepo/internal/store"
)

// Triggers, in increasing priority.
const (
	TriggerPatrol = "patrol"
	TriggerRetry  = "retry"
	TriggerPush   = "push"
	TriggerManual = "manual"
)

// leaseTTL bounds how long a crashed process can block a repository.
const leaseTTL = 2 * time.Minute

var triggerRank = map[string]int{TriggerPatrol: 1, TriggerRetry: 2, TriggerPush: 3, TriggerManual: 4}

// Request asks for a sync of a repository.
type Request struct {
	Trigger string
	// Overwrite replaces divergent refs (after backing them up) for this run
	// only, regardless of each remote's on_divergence setting.
	Overwrite bool
	// RemoteIDs restricts the run to these remotes; empty means all.
	RemoteIDs []int64
}

// Event reports progress of a run to waiters (e.g. a post-receive hook).
type Event struct {
	Type      string   `json:"type"` // start | remote | done
	Total     int      `json:"total,omitempty"`
	Remote    string   `json:"remote,omitempty"`
	State     string   `json:"state,omitempty"`
	Changed   int      `json:"changed,omitempty"`
	Error     string   `json:"error,omitempty"`
	Divergent []string `json:"divergent,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

type Options struct {
	Workers     int
	PushTimeout time.Duration
	RetryBase   time.Duration
	RetryMax    time.Duration
	// AlertAfterFailures emits remote.failed when a mirror reaches this many
	// consecutive failures (default 3).
	AlertAfterFailures int
	// PublicURL prefixes clone URLs in events.
	PublicURL string
	// Notify receives events (may be nil).
	Notify func(notify.Event)
}

type Manager struct {
	store  *store.Store
	git    *gitexec.Git
	box    *secrets.Box
	reg    *registry.Registry
	log    *slog.Logger
	opts   Options
	holder string

	sem    chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	queues map[int64]*repoQueue
}

type repoQueue struct {
	running bool
	pending *pendingRun
}

type pendingRun struct {
	trigger string
	all     bool
	ids     map[int64]bool
	// Overwrite is decided per remote: coalescing a targeted
	// `--overwrite --remote N` with a push must not overwrite the others.
	overwriteAll bool
	overwriteIDs map[int64]bool
	waiters      []chan Event
}

func New(st *store.Store, g *gitexec.Git, box *secrets.Box, reg *registry.Registry, log *slog.Logger, opts Options) *Manager {
	if opts.AlertAfterFailures <= 0 {
		opts.AlertAfterFailures = 3
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		store: st, git: g, box: box, reg: reg, log: log, opts: opts,
		holder: fmt.Sprintf("pid-%d-%d", pid(), time.Now().UnixNano()),
		sem:    make(chan struct{}, opts.Workers),
		ctx:    ctx, cancel: cancel,
		queues: map[int64]*repoQueue{},
	}
}

// Enqueue schedules a run for repoID and returns a channel that receives the
// events of the run that will include this request. The channel is closed
// when that run finishes. Slow readers miss events rather than block syncs.
func (m *Manager) Enqueue(repoID int64, req Request) <-chan Event {
	ch := make(chan Event, 256)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		close(ch)
		return ch
	}
	q := m.queues[repoID]
	if q == nil {
		q = &repoQueue{}
		m.queues[repoID] = q
	}
	if q.pending == nil {
		q.pending = &pendingRun{trigger: req.Trigger, ids: map[int64]bool{}, overwriteIDs: map[int64]bool{}}
	}
	p := q.pending
	if triggerRank[req.Trigger] > triggerRank[p.trigger] {
		p.trigger = req.Trigger
	}
	if len(req.RemoteIDs) == 0 {
		p.all = true
		p.overwriteAll = p.overwriteAll || req.Overwrite
	}
	for _, id := range req.RemoteIDs {
		p.ids[id] = true
		if req.Overwrite {
			p.overwriteIDs[id] = true
		}
	}
	p.waiters = append(p.waiters, ch)
	if !q.running {
		q.running = true
		m.wg.Add(1)
		go m.loop(repoID)
	}
	return ch
}

// EnqueueAll schedules every repository.
func (m *Manager) EnqueueAll(ctx context.Context, trigger string) error {
	repos, err := m.store.ListRepos(ctx)
	if err != nil {
		return err
	}
	for _, r := range repos {
		m.Enqueue(r.ID, Request{Trigger: trigger})
	}
	return nil
}

// Busy reports whether a run is active or queued for repoID.
func (m *Manager) Busy(repoID int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.queues[repoID]
	return q != nil && (q.running || q.pending != nil)
}

// Shutdown cancels in-flight runs and waits for them to stop.
func (m *Manager) Shutdown() {
	m.mu.Lock() // no Enqueue can start a loop after this
	m.cancel()
	m.mu.Unlock()
	m.wg.Wait()
}

// requeue schedules p again after a delay (without its waiters), keeping
// its scope and per-remote overwrite decisions.
func (m *Manager) requeue(repoID int64, p *pendingRun, delay time.Duration) {
	time.AfterFunc(delay, func() {
		if m.ctx.Err() != nil {
			return
		}
		var plain, over []int64
		for id := range p.ids {
			if p.overwriteIDs[id] {
				over = append(over, id)
			} else {
				plain = append(plain, id)
			}
		}
		if p.all {
			m.Enqueue(repoID, Request{Trigger: p.trigger, Overwrite: p.overwriteAll})
		} else if len(plain) > 0 {
			m.Enqueue(repoID, Request{Trigger: p.trigger, RemoteIDs: plain})
		}
		if len(over) > 0 && !p.overwriteAll {
			m.Enqueue(repoID, Request{Trigger: p.trigger, Overwrite: true, RemoteIDs: over})
		}
	})
}

func (m *Manager) loop(repoID int64) {
	defer m.wg.Done()
	for {
		m.mu.Lock()
		q := m.queues[repoID]
		p := q.pending
		q.pending = nil
		if p == nil || m.ctx.Err() != nil {
			q.running = false
			delete(m.queues, repoID)
			m.mu.Unlock()
			if p != nil {
				for _, w := range p.waiters {
					close(w)
				}
			}
			return
		}
		m.mu.Unlock()
		m.runRepo(repoID, p)
	}
}

func (m *Manager) runRepo(repoID int64, p *pendingRun) {
	emit := func(e Event) {
		for _, w := range p.waiters {
			select {
			case w <- e:
			default:
			}
		}
	}
	defer func() {
		for _, w := range p.waiters {
			close(w)
		}
	}()
	ctx := m.ctx
	log := m.log.With("repo_id", repoID, "trigger", p.trigger)

	// The lease is short and renewed while the run is alive, so a crashed
	// holder blocks the repository for at most leaseTTL.
	leaseName := "repo:" + strconv.FormatInt(repoID, 10)
	ok, err := m.store.AcquireLease(ctx, leaseName, m.holder, leaseTTL.Milliseconds())
	if err != nil || !ok {
		msg := "another repohub instance holds the sync lease; retrying in 30s"
		if err != nil {
			msg = err.Error() + "; retrying in 30s"
		}
		log.Warn("defer run", "reason", msg)
		m.requeue(repoID, p, 30*time.Second)
		emit(Event{Type: "done", Error: msg})
		return
	}
	renewDone := make(chan struct{})
	go func() {
		t := time.NewTicker(leaseTTL / 3)
		defer t.Stop()
		for {
			select {
			case <-renewDone:
				return
			case <-t.C:
				if _, err := m.store.AcquireLease(context.Background(), leaseName, m.holder, leaseTTL.Milliseconds()); err != nil {
					log.Warn("renew sync lease", "err", err)
				}
			}
		}
	}()
	defer func() {
		close(renewDone)
		m.store.ReleaseLease(context.Background(), leaseName, m.holder)
	}()

	repo, err := m.store.GetRepo(ctx, repoID)
	if err != nil {
		emit(Event{Type: "done", Error: err.Error()})
		return
	}
	log = log.With("repo", repo.Slug)
	all, err := m.store.ListRemotes(ctx, repoID)
	if err != nil {
		emit(Event{Type: "done", Error: err.Error()})
		return
	}
	var remotes []*store.Remote
	for _, r := range all {
		if p.all || p.ids[r.ID] {
			remotes = append(remotes, r)
		}
	}
	if len(remotes) == 0 {
		emit(Event{Type: "done"})
		return
	}
	hub, err := m.git.HubRefs(ctx, m.reg.Path(repo.Slug))
	if err != nil {
		log.Error("read hub refs", "err", err)
		emit(Event{Type: "done", Error: "read hub refs: " + err.Error()})
		return
	}
	emit(Event{Type: "start", Total: len(remotes)})

	var wg sync.WaitGroup
	var mu sync.Mutex
	var outcomes []RemoteOutcome
	for _, rem := range remotes {
		wg.Add(1)
		go func(rem *store.Remote) {
			defer wg.Done()
			select {
			case m.sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-m.sem }()
			req := Request{Trigger: p.trigger, Overwrite: p.overwriteAll || p.overwriteIDs[rem.ID]}
			out := m.syncRemote(ctx, repo, rem, hub, req)
			if out.State == store.StateFailed {
				log.Warn("remote sync failed", "remote", out.Label, "err", out.Error)
			} else {
				log.Info("remote synced", "remote", out.Label, "state", out.State, "changed", out.Changed)
			}
			m.notifyTransition(repo, rem, out)
			mu.Lock()
			outcomes = append(outcomes, out)
			mu.Unlock()
			emit(Event{Type: "remote", Remote: out.Label, State: out.State, Changed: out.Changed,
				Error: out.Error, Divergent: out.Divergent, Warnings: out.Warnings})
		}(rem)
	}
	wg.Wait()
	m.notifyCompleted(repo, p.trigger, outcomes)
	emit(Event{Type: "done"})
}

func (m *Manager) cloneURL(repo *store.Repo) string {
	if m.opts.PublicURL == "" {
		return ""
	}
	return m.opts.PublicURL + "/" + repo.Slug + ".git"
}

// notifyTransition emits per-mirror events on meaningful state changes only,
// so periodic patrols do not repeat alerts.
func (m *Manager) notifyTransition(repo *store.Repo, rem *store.Remote, out RemoteOutcome) {
	if m.opts.Notify == nil {
		return
	}
	base := notify.Event{Repo: repo.Slug, CloneURL: m.cloneURL(repo), Remote: out.Label, State: out.State}
	alerted := (rem.State == store.StateFailed && rem.ConsecutiveFailures >= m.opts.AlertAfterFailures) || rem.State == store.StateDiverged
	switch {
	case out.State == store.StateDiverged && rem.State != store.StateDiverged:
		base.Event = notify.EventRemoteDiverged
		base.Divergent = out.Divergent
	case out.State == store.StateFailed && rem.ConsecutiveFailures+1 == m.opts.AlertAfterFailures:
		base.Event = notify.EventRemoteFailed
		base.Error = out.Error
		base.Failures = rem.ConsecutiveFailures + 1
	case out.State == store.StateSynced && alerted:
		base.Event = notify.EventRemoteRecovered
	default:
		return
	}
	m.opts.Notify(base)
}

// notifyCompleted emits sync.completed when any mirror received changes.
func (m *Manager) notifyCompleted(repo *store.Repo, trigger string, outcomes []RemoteOutcome) {
	if m.opts.Notify == nil {
		return
	}
	refs := map[string]string{}
	var summaries []notify.RemoteSummary
	for _, o := range outcomes {
		for ref, sha := range o.Updated {
			refs[ref] = sha
		}
		summaries = append(summaries, notify.RemoteSummary{Remote: o.Label, State: o.State, Changed: o.Changed, Error: o.Error, Divergent: o.Divergent})
	}
	if len(refs) == 0 {
		return
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].Remote < summaries[j].Remote })
	m.opts.Notify(notify.Event{
		Event: notify.EventSyncCompleted, Repo: repo.Slug, CloneURL: m.cloneURL(repo),
		Trigger: trigger, Refs: refs, Remotes: summaries,
	})
}

func (m *Manager) backoff(failures int) time.Duration {
	d := m.opts.RetryBase
	for i := 0; i < failures && d < m.opts.RetryMax; i++ {
		d *= 2
	}
	if d > m.opts.RetryMax {
		d = m.opts.RetryMax
	}
	return d
}
