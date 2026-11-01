package edge

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/qwq-inc/gitrepo/internal/registry"
)

// A change that arrives after a worker's fetch, while it is deciding to go
// idle, must still be fetched: schedule only marks a busy repository dirty,
// so the worker has to look at that flag where it marks itself idle.
func TestChangeArrivingAfterFetchIsNotLost(t *testing.T) {
	r := &Replica{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Registry: &registry.Registry{ReposDir: t.TempDir()}}
	r.repos = map[string]*repoState{"acme/app": {}}
	r.queue = make(chan string, 8)
	var fetches atomic.Int32
	r.fetchFunc = func(context.Context, string, string) error {
		fetches.Add(1)
		return errors.New("stub") // keeps the database out of the test
	}
	var once atomic.Bool
	r.testAfterFetch = func() {
		if once.CompareAndSwap(false, true) {
			r.schedule("acme/app") // a new hub state arrives right now
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.schedule("acme/app")
	r.run(ctx, <-r.queue)
	cancel() // stop the retry timer
	if n := fetches.Load(); n != 2 {
		t.Fatalf("fetches: %d, want 2 (the change after the first fetch was lost)", n)
	}
}
