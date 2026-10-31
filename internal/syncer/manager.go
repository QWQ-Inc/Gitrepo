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
	"strconv"
	"sync"
	"time"

	"github.com/qwq-inc/gitrepo/internal/gitexec"
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
	trigger   string
	overwrite bool
	all       bool
	ids       map[int64]bool
	waiters   []chan Event
}

func New(st *store.Store, g *gitexec.Git, box *secrets.Box, reg *registry.Registry, log *slog.Logger, opts Options) *Manager {
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
		q.pending = &pendingRun{trigger: req.Trigger, ids: map[int64]bool{}}
	}
	p := q.pending
	if triggerRank[req.Trigger] > triggerRank[p.trigger] {
		p.trigger = req.Trigger
	}
	p.overwrite = p.overwrite || req.Overwrite
	if len(req.RemoteIDs) == 0 {
		p.all = true
	}
	for _, id := range req.RemoteIDs {
		p.ids[id] = true
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
	m.cancel()
	m.wg.Wait()
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

	leaseName := "repo:" + strconv.FormatInt(repoID, 10)
	ttl := (2*m.opts.PushTimeout + time.Minute).Milliseconds()
	ok, err := m.store.AcquireLease(ctx, leaseName, m.holder, ttl)
	if err != nil || !ok {
		msg := "another repohub instance holds the sync lease"
		if err != nil {
			msg = err.Error()
		}
		log.Warn("skip run", "reason", msg)
		emit(Event{Type: "done", Error: msg})
		return
	}
	defer m.store.ReleaseLease(context.Background(), leaseName, m.holder)

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
	req := Request{Trigger: p.trigger, Overwrite: p.overwrite}

	var wg sync.WaitGroup
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
			out := m.syncRemote(ctx, repo, rem, hub, req)
			if out.State == store.StateFailed {
				log.Warn("remote sync failed", "remote", out.Label, "err", out.Error)
			} else {
				log.Info("remote synced", "remote", out.Label, "state", out.State, "changed", out.Changed)
			}
			emit(Event{Type: "remote", Remote: out.Label, State: out.State, Changed: out.Changed,
				Error: out.Error, Divergent: out.Divergent, Warnings: out.Warnings})
		}(rem)
	}
	wg.Wait()
	emit(Event{Type: "done"})
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
