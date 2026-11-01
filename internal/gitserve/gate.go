package gitserve

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrMaintenance is returned to pushes while a repository is being purged.
var ErrMaintenance = errors.New("repository maintenance in progress (purging backups); retry in a minute")

// Gate lets maintenance that rewrites a repository's object store (a backup
// purge: git gc plus LFS cleanup) run while no push is in flight. Pushes
// register with BeginPush; Maintain refuses new pushes and waits for the
// running ones to finish.
type Gate struct {
	mu     sync.Mutex
	pushes map[int64]int
	maint  map[int64]bool
}

// BeginPush registers a push to the repository. It fails while maintenance
// runs; otherwise done must be called when the push has finished.
func (g *Gate) BeginPush(repoID int64) (done func(), err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.maint[repoID] {
		return nil, ErrMaintenance
	}
	if g.pushes == nil {
		g.pushes = map[int64]int{}
	}
	g.pushes[repoID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.pushes[repoID]--; g.pushes[repoID] <= 0 {
				delete(g.pushes, repoID)
			}
		})
	}, nil
}

// Maintaining reports whether pushes to the repository are being refused.
func (g *Gate) Maintaining(repoID int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.maint[repoID]
}

// Maintain refuses new pushes to the repository and waits until running
// pushes have finished. release reopens the repository.
func (g *Gate) Maintain(ctx context.Context, repoID int64) (release func(), err error) {
	g.mu.Lock()
	if g.maint[repoID] {
		g.mu.Unlock()
		return nil, errors.New("maintenance of this repository is already running")
	}
	if g.maint == nil {
		g.maint = map[int64]bool{}
	}
	g.maint[repoID] = true
	g.mu.Unlock()
	var once sync.Once
	release = func() {
		once.Do(func() {
			g.mu.Lock()
			delete(g.maint, repoID)
			g.mu.Unlock()
		})
	}
	for {
		g.mu.Lock()
		busy := g.pushes[repoID] > 0
		g.mu.Unlock()
		if !busy {
			return release, nil
		}
		select {
		case <-ctx.Done():
			release()
			return nil, errors.New("a push to this repository is still running; try again shortly")
		case <-time.After(50 * time.Millisecond):
		}
	}
}
