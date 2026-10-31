package syncer

import (
	"reflect"
	"testing"
)

// ancestry is a toy history: key is an ancestor of every value.
type ancestry map[string][]string

func (a ancestry) isAncestor(x, y string) bool {
	if x == y {
		return true
	}
	for _, d := range a[x] {
		if d == y {
			return true
		}
	}
	return false
}

func TestMakePlan(t *testing.T) {
	hist := ancestry{"A": {"B", "C"}, "B": {"C"}}
	cases := []struct {
		name      string
		hub       map[string]string
		remote    map[string]string
		pushed    map[string]string
		overwrite bool
		ops       []Op
		divs      []string
		inSync    int
	}{
		{
			name:   "in sync",
			hub:    map[string]string{"refs/heads/main": "A"},
			remote: map[string]string{"refs/heads/main": "A"},
			inSync: 1,
		},
		{
			name:   "create missing ref with must-not-exist lease",
			hub:    map[string]string{"refs/heads/main": "A"},
			remote: map[string]string{},
			ops:    []Op{{Ref: "refs/heads/main", Action: ActionCreate, New: "A"}},
		},
		{
			name:   "recreate ref deleted on mirror",
			hub:    map[string]string{"refs/heads/main": "A"},
			remote: map[string]string{},
			pushed: map[string]string{"refs/heads/main": "A"},
			ops:    []Op{{Ref: "refs/heads/main", Action: ActionCreate, New: "A"}},
		},
		{
			name:   "fast-forward our own ref",
			hub:    map[string]string{"refs/heads/main": "B"},
			remote: map[string]string{"refs/heads/main": "A"},
			pushed: map[string]string{"refs/heads/main": "A"},
			ops:    []Op{{Ref: "refs/heads/main", Action: ActionUpdate, New: "B", Expect: "A"}},
		},
		{
			name:   "force-update our own ref after hub rewrite",
			hub:    map[string]string{"refs/heads/main": "X"},
			remote: map[string]string{"refs/heads/main": "A"},
			pushed: map[string]string{"refs/heads/main": "A"},
			ops:    []Op{{Ref: "refs/heads/main", Action: ActionUpdate, New: "X", Expect: "A", Force: true}},
		},
		{
			name:   "fast-forward a ref we never pushed (adopt existing mirror)",
			hub:    map[string]string{"refs/heads/main": "C"},
			remote: map[string]string{"refs/heads/main": "A"},
			ops:    []Op{{Ref: "refs/heads/main", Action: ActionUpdate, New: "C", Expect: "A"}},
		},
		{
			name:   "foreign commit on mirror diverges",
			hub:    map[string]string{"refs/heads/main": "B"},
			remote: map[string]string{"refs/heads/main": "Z"},
			pushed: map[string]string{"refs/heads/main": "A"},
			divs:   []string{"refs/heads/main"},
		},
		{
			name:      "foreign commit is overwritten and flagged for backup",
			hub:       map[string]string{"refs/heads/main": "B"},
			remote:    map[string]string{"refs/heads/main": "Z"},
			pushed:    map[string]string{"refs/heads/main": "A"},
			overwrite: true,
			ops:       []Op{{Ref: "refs/heads/main", Action: ActionUpdate, New: "B", Expect: "Z", Force: true, Overwrite: true}},
		},
		{
			name:   "delete ref we pushed and the hub removed",
			hub:    map[string]string{},
			remote: map[string]string{"refs/heads/old": "A"},
			pushed: map[string]string{"refs/heads/old": "A"},
			ops:    []Op{{Ref: "refs/heads/old", Action: ActionDelete, Expect: "A", Force: true}},
		},
		{
			name:   "ref created directly on the mirror is reported, not deleted",
			hub:    map[string]string{},
			remote: map[string]string{"refs/heads/theirs": "Z"},
			divs:   []string{"refs/heads/theirs"},
		},
		{
			name:   "ref deleted on hub but changed on mirror diverges",
			hub:    map[string]string{},
			remote: map[string]string{"refs/heads/old": "Z"},
			pushed: map[string]string{"refs/heads/old": "A"},
			divs:   []string{"refs/heads/old"},
		},
		{
			name:   "tag moves always need force",
			hub:    map[string]string{"refs/tags/v1": "B"},
			remote: map[string]string{"refs/tags/v1": "A"},
			pushed: map[string]string{"refs/tags/v1": "A"},
			ops:    []Op{{Ref: "refs/tags/v1", Action: ActionUpdate, New: "B", Expect: "A", Force: true}},
		},
		{
			name:   "one divergent ref does not block the others",
			hub:    map[string]string{"refs/heads/main": "B", "refs/heads/dev": "C"},
			remote: map[string]string{"refs/heads/main": "Z", "refs/heads/dev": "B"},
			pushed: map[string]string{"refs/heads/main": "A", "refs/heads/dev": "B"},
			ops:    []Op{{Ref: "refs/heads/dev", Action: ActionUpdate, New: "C", Expect: "B"}},
			divs:   []string{"refs/heads/main"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := MakePlan(PlanInput{
				Hub: tc.hub, Remote: tc.remote, Pushed: orEmpty(tc.pushed),
				IsAncestor: hist.isAncestor, Overwrite: tc.overwrite,
			})
			if !reflect.DeepEqual(p.Ops, tc.ops) {
				t.Errorf("ops:\n got  %+v\n want %+v", p.Ops, tc.ops)
			}
			var divs []string
			for _, d := range p.Divergent {
				divs = append(divs, d.Ref)
			}
			if !reflect.DeepEqual(divs, tc.divs) {
				t.Errorf("divergent: got %v want %v", divs, tc.divs)
			}
			if p.InSync != tc.inSync {
				t.Errorf("in sync: got %d want %d", p.InSync, tc.inSync)
			}
		})
	}
}

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func TestBackoff(t *testing.T) {
	m := &Manager{opts: Options{RetryBase: 30_000_000_000, RetryMax: 3600_000_000_000}}
	if got := m.backoff(0); got.Seconds() != 30 {
		t.Fatalf("backoff(0) = %v", got)
	}
	if got := m.backoff(3); got.Seconds() != 240 {
		t.Fatalf("backoff(3) = %v", got)
	}
	if got := m.backoff(50); got.Hours() != 1 {
		t.Fatalf("backoff(50) = %v", got)
	}
}
