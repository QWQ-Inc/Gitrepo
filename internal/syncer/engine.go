package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/qwq-inc/gitrepo/internal/gitexec"
	"github.com/qwq-inc/gitrepo/internal/provider"
	"github.com/qwq-inc/gitrepo/internal/store"
)

func pid() int { return os.Getpid() }

// RemoteOutcome summarises one remote's sync.
type RemoteOutcome struct {
	Label     string
	State     string
	Changed   int
	Error     string
	Divergent []string
	Warnings  []string
}

// syncRemote brings one remote in line with the hub refs:
// provision → ls-remote → plan → back up → relax protection → push → verify.
func (m *Manager) syncRemote(parent context.Context, repo *store.Repo, rem *store.Remote, hub map[string]string, req Request) (out RemoteOutcome) {
	out.Label = rem.Label()
	run := &store.SyncRun{RepoID: repo.ID, RemoteID: rem.ID, Trigger: req.Trigger, Status: store.StateSyncing}
	_ = m.store.InsertRun(parent, run)
	_ = m.store.MarkSyncing(parent, rem.ID)
	var divs []Div
	var detail []string
	planned := false

	defer func() {
		now := store.Now()
		res := store.RemoteResult{State: out.State, Error: out.Error, SyncedAt: now}
		switch out.State {
		case store.StateSynced:
			res.Success = true
		case store.StateFailed:
			res.Failed = true
			res.NextRetryAt = now + m.backoff(rem.ConsecutiveFailures).Milliseconds()
		case store.StateBehind:
			res.NextRetryAt = now + m.opts.RetryBase.Milliseconds()
		}
		bg := context.Background()
		if err := m.store.RecordRemoteResult(bg, rem.ID, res); err != nil {
			m.log.Error("record remote result", "err", err)
		}
		if planned {
			sd := make([]store.Divergence, 0, len(divs))
			for _, d := range divs {
				sd = append(sd, store.Divergence{RemoteID: rem.ID, Ref: d.Ref, HubSHA: d.HubSHA, RemoteSHA: d.RemoteSHA, ExpectedSHA: d.Expected, Reason: d.Reason})
			}
			if err := m.store.ReplaceDivergences(bg, rem.ID, sd); err != nil {
				m.log.Error("record divergences", "err", err)
			}
		}
		for _, d := range divs {
			out.Divergent = append(out.Divergent, d.Ref)
		}
		run.Status = out.State
		run.RefsChanged = out.Changed
		run.Error = out.Error
		run.Detail = strings.Join(append(detail, out.Warnings...), "\n")
		_ = m.store.FinishRun(bg, run)
	}()

	fail := func(format string, args ...any) RemoteOutcome {
		out.State = store.StateFailed
		out.Error = fmt.Sprintf(format, args...)
		return out
	}

	ctx, cancel := context.WithTimeout(parent, m.opts.PushTimeout)
	defer cancel()

	t, err := m.store.GetTarget(ctx, rem.TargetID)
	if err != nil {
		return fail("load target: %v", err)
	}
	prov, err := provider.Get(t.Provider)
	if err != nil {
		return fail("%v", err)
	}
	secret := ""
	if len(t.CredSecret) > 0 {
		pt, err := m.box.Open(t.CredSecret, t.CredentialAAD())
		if err != nil {
			return fail("target %s: %v", t.Name, err)
		}
		secret = string(pt)
	}
	cred := prov.GitCred(t, secret)
	url := rem.URL
	if url == "" {
		if url, err = prov.RepoURL(t, rem.Namespace, rem.Name); err != nil {
			return fail("%v", err)
		}
	}
	spec := provider.RepoSpec{Namespace: rem.Namespace, Name: rem.Name, Visibility: repo.Visibility}
	if t.Visibility != "" {
		spec.Visibility = t.Visibility
	}
	repoPath := m.reg.Path(repo.Slug)

	// 1. Provision the remote repository on first use.
	created := false
	if rem.Enabled && !rem.Provisioned {
		if prov.Caps().CreateRepo && t.AutoCreate {
			if created, err = prov.EnsureRepo(ctx, t, secret, spec); err != nil {
				return fail("create remote repository: %v", err)
			}
			if created {
				detail = append(detail, "created remote repository")
			}
		}
		if err := m.store.SetRemoteProvisioned(ctx, rem.ID, true); err != nil {
			return fail("%v", err)
		}
	}

	// 2. Observe the remote.
	remoteRefs, err := m.git.LsRemote(ctx, url, cred)
	if err != nil {
		if prov.Caps().CreateRepo && t.AutoCreate && looksMissing(err) {
			// The repository vanished (or was never created); try again next time.
			_ = m.store.SetRemoteProvisioned(ctx, rem.ID, false)
		}
		return fail("ls-remote: %v", err)
	}
	_ = m.store.ReplaceRefState(ctx, rem.ID, remoteRefs)
	pushed, err := m.store.GetPushedState(ctx, rem.ID)
	if err != nil {
		return fail("%v", err)
	}

	// 3. Plan.
	plan := MakePlan(PlanInput{
		Hub: hub, Remote: remoteRefs, Pushed: pushed,
		IsAncestor: func(a, b string) bool { return m.git.IsAncestor(ctx, repoPath, a, b) },
		Overwrite:  rem.OnDivergence == store.OnDivergenceOverwrite || req.Overwrite,
	})
	divs = append(divs, plan.Divergent...)
	planned = true

	if !rem.Enabled {
		out.State = store.StatePaused
		if len(plan.Ops) > 0 || len(divs) > 0 {
			out.Warnings = append(out.Warnings, fmt.Sprintf("paused: %d ref(s) out of date", len(plan.Ops)+len(divs)))
		}
		return out
	}

	ops := plan.Ops
	failures := map[string]string{}

	// 4. Back up remote data that is about to be overwritten.
	var toBackup []Op
	for _, op := range ops {
		if op.Overwrite {
			toBackup = append(toBackup, op)
		}
	}
	if len(toBackup) > 0 {
		errs := m.backupRemote(ctx, repoPath, url, cred, rem.ID, toBackup)
		if len(errs) > 0 {
			kept := ops[:0]
			for _, op := range ops {
				if e, bad := errs[op.Ref]; bad {
					failures[op.Ref] = "backup before overwrite failed: " + e.Error()
					continue
				}
				kept = append(kept, op)
			}
			ops = kept
		} else {
			detail = append(detail, fmt.Sprintf("backed up %d divergent ref(s) under refs/backup/", len(toBackup)))
		}
	}

	// 5. Relax branch protection where a forced update or delete is needed.
	if prov.Caps().BranchProtection {
		var force, del []string
		for _, op := range ops {
			if !strings.HasPrefix(op.Ref, "refs/heads/") {
				continue
			}
			b := strings.TrimPrefix(op.Ref, "refs/heads/")
			if op.Action == ActionDelete {
				del = append(del, b)
			} else if op.Force {
				force = append(force, b)
			}
		}
		if err := prov.AllowForce(ctx, t, secret, spec, force, del); err != nil {
			out.Warnings = append(out.Warnings, "relax branch protection: "+err.Error())
		}
	}

	// 6. Push.
	if len(ops) > 0 {
		statuses, err := m.push(ctx, repoPath, url, cred, rem, ops)
		if err != nil {
			return fail("push: %v", err)
		}
		for _, op := range ops {
			st := statuses[op.Ref]
			switch {
			case st.OK():
				if st.Flag != '=' {
					out.Changed++
				}
			case st.StaleLease():
				divs = append(divs, Div{Ref: op.Ref, HubSHA: op.New, Expected: op.Expect, Reason: "remote changed during sync (lease rejected)"})
			default:
				failures[op.Ref] = strings.TrimSpace(st.Summary + " " + st.Reason)
			}
		}
	}

	// 7. Verify.
	after, err := m.git.LsRemote(ctx, url, cred)
	if err != nil {
		return fail("verify (ls-remote): %v", err)
	}
	_ = m.store.ReplaceRefState(ctx, rem.ID, after)
	set := map[string]string{}
	var del []string
	for ref, sha := range hub {
		if after[ref] == sha && pushed[ref] != sha {
			set[ref] = sha
		}
	}
	for ref := range pushed {
		_, inHub := hub[ref]
		_, onRemote := after[ref]
		if !inHub && !onRemote {
			del = append(del, ref)
		}
	}
	if err := m.store.ApplyPushedState(ctx, rem.ID, set, del); err != nil {
		return fail("record pushed state: %v", err)
	}
	for i := range divs {
		divs[i].RemoteSHA = after[divs[i].Ref]
	}

	mismatch := false
	for ref, sha := range hub {
		if after[ref] != sha {
			mismatch = true
		}
	}
	for ref := range after {
		if _, ok := hub[ref]; !ok {
			mismatch = true
		}
	}

	if created && repo.DefaultBranch != "" && prov.Caps().DefaultBranch && after["refs/heads/"+repo.DefaultBranch] != "" {
		if err := prov.SetDefaultBranch(ctx, t, secret, spec, repo.DefaultBranch); err != nil {
			out.Warnings = append(out.Warnings, "set default branch: "+err.Error())
		}
	}

	switch {
	case len(failures) > 0:
		out.State = store.StateFailed
		refs := make([]string, 0, len(failures))
		for ref := range failures {
			refs = append(refs, ref)
		}
		sort.Strings(refs)
		var parts []string
		for _, ref := range refs {
			parts = append(parts, ref+": "+failures[ref])
		}
		out.Error = "rejected: " + strings.Join(parts, "; ")
	case len(divs) > 0:
		out.State = store.StateDiverged
	case mismatch:
		out.State = store.StateBehind
	default:
		out.State = store.StateSynced
	}
	return out
}

// push sends ops, falling back to non-atomic pushes for servers without
// atomic support, and retrying the innocent refs of a failed atomic push.
func (m *Manager) push(ctx context.Context, repoPath, url string, cred *gitexec.Cred, rem *store.Remote, ops []Op) (map[string]gitexec.RefStatus, error) {
	atomic := !rem.AtomicUnsupported
	final := map[string]gitexec.RefStatus{}
	for attempt := 0; attempt < 4 && len(ops) > 0; attempt++ {
		pushOps := make([]gitexec.PushOp, 0, len(ops))
		for _, op := range ops {
			pushOps = append(pushOps, gitexec.PushOp{Ref: op.Ref, New: op.New, Expect: op.Expect})
		}
		res, err := m.git.Push(ctx, repoPath, url, cred, pushOps, atomic)
		if errors.Is(err, gitexec.ErrAtomicUnsupported) && atomic {
			_ = m.store.SetRemoteAtomicUnsupported(ctx, rem.ID)
			atomic = false
			continue
		}
		if err != nil {
			return final, err
		}
		var retry []Op
		for _, op := range ops {
			st, ok := res.Refs[op.Ref]
			if !ok {
				st = gitexec.RefStatus{Ref: op.Ref, Flag: '!', Summary: "[no status]", Reason: "not reported by git push"}
			}
			if atomic && st.AtomicCollateral() {
				retry = append(retry, op)
				continue
			}
			final[op.Ref] = st
		}
		if len(retry) == len(ops) {
			// Every ref failed only as collateral; nothing to isolate.
			for _, op := range retry {
				final[op.Ref] = res.Refs[op.Ref]
			}
			break
		}
		ops = retry
	}
	return final, nil
}

func looksMissing(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not found") || strings.Contains(s, "does not exist") || strings.Contains(s, "could not be found")
}

// ---------------------------------------------------------------- backups

// stampLayout names backup refs: refs/backup/<stamp>/<ref>.
const stampLayout = "20060102T150405.000Z"

func backupStamp(t time.Time) string { return t.UTC().Format(stampLayout) }

// backupRemote stores the remote values of refs about to be overwritten
// under refs/backup/<stamp>/remotes/<remote-id>/<ref> in the hub.
func (m *Manager) backupRemote(ctx context.Context, repoPath, url string, cred *gitexec.Cred, remoteID int64, ops []Op) map[string]error {
	stamp := backupStamp(time.Now())
	errs := map[string]error{}
	var updates, specs []string
	var fetched []Op
	for _, op := range ops {
		bref := fmt.Sprintf("refs/backup/%s/remotes/%d/%s", stamp, remoteID, op.Ref)
		if m.git.HasObject(ctx, repoPath, op.Expect) {
			updates = append(updates, "update "+bref+" "+op.Expect)
		} else {
			specs = append(specs, "+"+op.Ref+":"+bref)
			fetched = append(fetched, op)
		}
	}
	if len(specs) > 0 {
		if err := m.git.Fetch(ctx, repoPath, url, cred, specs...); err != nil {
			for _, op := range fetched {
				errs[op.Ref] = err
			}
		}
	}
	if err := m.git.UpdateRefs(ctx, repoPath, updates); err != nil {
		for _, op := range ops {
			if _, done := errs[op.Ref]; !done {
				errs[op.Ref] = err
			}
		}
	}
	return errs
}

// RefUpdate is one line of post-receive input.
type RefUpdate struct {
	Old string `json:"old"`
	New string `json:"new"`
	Ref string `json:"ref"`
}

// BackupHubUpdates preserves hub values that a client push overwrote
// (non-fast-forward, tag move) or deleted.
func BackupHubUpdates(ctx context.Context, g *gitexec.Git, repoPath string, updates []RefUpdate) (int, error) {
	stamp := backupStamp(time.Now())
	var lines []string
	for _, u := range updates {
		if gitexec.IsZero(u.Old) {
			continue
		}
		overwrite := gitexec.IsZero(u.New) || strings.HasPrefix(u.Ref, "refs/tags/") || !g.IsAncestor(ctx, repoPath, u.Old, u.New)
		if overwrite {
			lines = append(lines, "update refs/backup/"+stamp+"/"+u.Ref+" "+u.Old)
		}
	}
	return len(lines), g.UpdateRefs(ctx, repoPath, lines)
}

// PruneBackups deletes backup refs older than retention.
func PruneBackups(ctx context.Context, g *gitexec.Git, repoPath string, retention time.Duration) (int, error) {
	refs, err := g.ForEachRef(ctx, repoPath, "refs/backup/")
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-retention)
	var lines []string
	for ref := range refs {
		stamp, _, _ := strings.Cut(strings.TrimPrefix(ref, "refs/backup/"), "/")
		ts, err := time.Parse(stampLayout, stamp)
		if err == nil && ts.Before(cutoff) {
			lines = append(lines, "delete "+ref)
		}
	}
	return len(lines), g.UpdateRefs(ctx, repoPath, lines)
}
