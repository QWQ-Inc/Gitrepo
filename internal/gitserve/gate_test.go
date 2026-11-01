package gitserve

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGate(t *testing.T) {
	var g Gate
	done, err := g.BeginPush(1)
	if err != nil {
		t.Fatal(err)
	}
	// Maintenance waits for the running push.
	got := make(chan func(), 1)
	go func() {
		release, err := g.Maintain(context.Background(), 1)
		if err != nil {
			t.Error(err)
		}
		got <- release
	}()
	time.Sleep(150 * time.Millisecond)
	select {
	case <-got:
		t.Fatal("maintenance started while a push was running")
	default:
	}
	if _, err := g.BeginPush(1); !errors.Is(err, ErrMaintenance) {
		t.Fatalf("new push during maintenance: %v", err)
	}
	if _, err := g.BeginPush(2); err != nil {
		t.Fatalf("other repositories must not be blocked: %v", err)
	}
	done()
	done() // idempotent
	release := <-got
	if _, err := g.Maintain(context.Background(), 1); err == nil {
		t.Fatal("two maintenance tasks on one repository")
	}
	release()
	if d, err := g.BeginPush(1); err != nil {
		t.Fatalf("push after maintenance: %v", err)
	} else {
		d()
	}

	// A push that does not finish makes maintenance give up, reopened.
	d, _ := g.BeginPush(3)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := g.Maintain(ctx, 3); err == nil {
		t.Fatal("maintenance did not time out")
	}
	if g.Maintaining(3) {
		t.Fatal("a timed-out maintenance kept the repository closed")
	}
	d()
}
