package syncer

import (
	"context"
	"testing"
)

// A targeted overwrite must stay targeted when it is coalesced with a push
// (or patrol) that covers every remote.
func TestOverwriteStaysPerRemote(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &Manager{ctx: ctx, queues: map[int64]*repoQueue{1: {running: true}}}
	m.Enqueue(1, Request{Trigger: TriggerManual, Overwrite: true, RemoteIDs: []int64{5}})
	m.Enqueue(1, Request{Trigger: TriggerPush})
	p := m.queues[1].pending
	if !p.all || p.overwriteAll || !p.overwriteIDs[5] || len(p.overwriteIDs) != 1 {
		t.Fatalf("pending run: all=%v overwriteAll=%v overwriteIDs=%v", p.all, p.overwriteAll, p.overwriteIDs)
	}
	if p.trigger != TriggerManual || len(p.waiters) != 2 {
		t.Fatalf("trigger %s, %d waiters", p.trigger, len(p.waiters))
	}
	m.Enqueue(1, Request{Trigger: TriggerManual, Overwrite: true})
	if !p.overwriteAll {
		t.Fatal("repo-wide overwrite was lost")
	}
}
