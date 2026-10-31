package syncer

import (
	"sort"
	"strings"
)

// Action is what a sync does to one remote ref.
type Action string

const (
	ActionCreate Action = "create"
	ActionUpdate Action = "update"
	ActionDelete Action = "delete"
)

// Op is a planned ref change on a remote.
type Op struct {
	Ref    string
	Action Action
	New    string // target SHA ("" for delete)
	// Expect is the remote's current value, used as the force-with-lease
	// expectation ("" = ref must not exist).
	Expect string
	// Force: the update is not a fast-forward (or is a delete / tag move), so
	// branch protection may have to be relaxed.
	Force bool
	// Overwrite: the remote value was not written by Gitrepo and is being
	// replaced because of on_divergence=overwrite; it must be backed up first.
	Overwrite bool
}

// Div describes a ref where the remote holds changes Gitrepo did not make.
type Div struct {
	Ref       string
	HubSHA    string
	RemoteSHA string
	Expected  string // last SHA Gitrepo pushed ("" if never)
	Reason    string
}

// Plan is the outcome of comparing hub, remote and pushed state.
type Plan struct {
	Ops       []Op
	Divergent []Div
	InSync    int
}

// PlanInput feeds MakePlan.
type PlanInput struct {
	Hub    map[string]string // refs on the hub (the source of truth)
	Remote map[string]string // refs observed on the remote just now
	Pushed map[string]string // what Gitrepo last pushed to the remote
	// IsAncestor reports whether a is an ancestor of b in the hub repository
	// (false if either object is unknown to the hub).
	IsAncestor func(a, b string) bool
	// Overwrite replaces divergent refs instead of reporting them.
	Overwrite bool
}

// MakePlan decides, per ref, how to bring the remote in line with the hub
// without ever silently destroying data Gitrepo did not write:
//
//   - remote == hub                         → nothing to do
//   - remote missing                        → create (re-creating a ref someone
//     deleted on the mirror loses nothing)
//   - remote == last pushed                 → update/delete; it is ours
//   - remote is an ancestor of hub          → fast-forward; loses nothing
//     (branches only: a foreign tag is never moved)
//   - otherwise                             → divergent: alert, or back up and
//     overwrite when the remote is configured to do so
//
// Every op carries the observed remote value as its lease, so a change that
// lands between observation and push is rejected by the remote and surfaces
// as a divergence on the next pass.
func MakePlan(in PlanInput) Plan {
	refs := map[string]struct{}{}
	for r := range in.Hub {
		refs[r] = struct{}{}
	}
	for r := range in.Remote {
		refs[r] = struct{}{}
	}
	names := make([]string, 0, len(refs))
	for r := range refs {
		names = append(names, r)
	}
	sort.Strings(names)

	var p Plan
	for _, ref := range names {
		h, r, pushed := in.Hub[ref], in.Remote[ref], in.Pushed[ref]
		isTag := strings.HasPrefix(ref, "refs/tags/")
		switch {
		case h != "" && r == h:
			p.InSync++

		case h != "" && r == "":
			p.Ops = append(p.Ops, Op{Ref: ref, Action: ActionCreate, New: h})

		case h != "" && r != "":
			switch {
			case r == pushed:
				p.Ops = append(p.Ops, Op{Ref: ref, Action: ActionUpdate, New: h, Expect: r, Force: isTag || !in.IsAncestor(r, h)})
			case !isTag && in.IsAncestor(r, h):
				// A fast-forward keeps every commit. Tags are excluded: moving
				// someone else's tag (e.g. a release) loses what it named.
				p.Ops = append(p.Ops, Op{Ref: ref, Action: ActionUpdate, New: h, Expect: r})
			case in.Overwrite:
				p.Ops = append(p.Ops, Op{Ref: ref, Action: ActionUpdate, New: h, Expect: r, Force: true, Overwrite: true})
			default:
				reason := "remote has commits the hub does not"
				switch {
				case isTag:
					reason = "tag on the remote points elsewhere and was not written by Gitrepo"
				case pushed == "":
					reason = "remote ref was not created by Gitrepo"
				}
				p.Divergent = append(p.Divergent, Div{Ref: ref, HubSHA: h, RemoteSHA: r, Expected: pushed, Reason: reason})
			}

		case h == "" && r != "":
			switch {
			case r == pushed:
				p.Ops = append(p.Ops, Op{Ref: ref, Action: ActionDelete, Expect: r, Force: true})
			case in.Overwrite:
				p.Ops = append(p.Ops, Op{Ref: ref, Action: ActionDelete, Expect: r, Force: true, Overwrite: true})
			default:
				reason := "ref exists only on the remote"
				if pushed != "" {
					reason = "ref was deleted on the hub but changed on the remote"
				}
				p.Divergent = append(p.Divergent, Div{Ref: ref, RemoteSHA: r, Expected: pushed, Reason: reason})
			}
		}
	}
	return p
}
