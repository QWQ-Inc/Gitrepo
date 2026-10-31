package syncer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qwq-inc/gitrepo/internal/lfs"
	"github.com/qwq-inc/gitrepo/internal/store"
)

// ErrHubNotEmpty means an import would overwrite existing hub refs.
var ErrHubNotEmpty = errors.New("the hub repository already has branches or tags")

// ErrConcurrentChange means a push changed the hub during an import.
var ErrConcurrentChange = errors.New("concurrent change")

func orZero(sha string) string {
	if sha == "" {
		return "0000000000000000000000000000000000000000"
	}
	return sha
}

// ImportResult summarises an import.
type ImportResult struct {
	Refs          int      `json:"refs"`
	LFSObjects    int      `json:"lfs_objects"`
	DefaultBranch string   `json:"default_branch,omitempty"`
	BackedUp      int      `json:"backed_up,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

// Import makes the hub repository a copy of one of its mirrors: every
// branch and tag (and the LFS objects they reference) is fetched from rem,
// HEAD follows the mirror's default branch, and rem is recorded as adopted
// and in sync. It is how an existing project moves onto Gitrepo, and how a
// lost hub is rebuilt. Without force it refuses to touch a hub that already
// has refs; with force, existing refs are backed up and replaced.
//
// Import holds the repository's sync lease, so it never interleaves with a
// sync run. Afterwards a sync of the remaining mirrors is queued.
func (m *Manager) Import(ctx context.Context, repo *store.Repo, rem *store.Remote, force bool) (*ImportResult, error) {
	leaseName := "repo:" + strconv.FormatInt(repo.ID, 10)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		ok, err := m.store.AcquireLease(ctx, leaseName, m.holder+"-import", m.opts.PushTimeout.Milliseconds()+leaseTTL.Milliseconds())
		if err != nil {
			return nil, err
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			return nil, errors.New("a sync of this repository is running; try again shortly")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	defer m.store.ReleaseLease(context.Background(), leaseName, m.holder+"-import")

	ctx, cancel := context.WithTimeout(ctx, m.opts.PushTimeout)
	defer cancel()
	rc, err := m.resolveRemote(ctx, repo, rem)
	if err != nil {
		return nil, err
	}
	repoPath := m.reg.Path(repo.Slug)
	res := &ImportResult{}

	before, err := m.git.HubRefs(ctx, repoPath)
	if err != nil {
		return nil, err
	}
	if len(before) > 0 {
		if !force {
			return nil, fmt.Errorf("%w (%d refs); import with force to replace them (they are backed up first)", ErrHubNotEmpty, len(before))
		}
		stamp := backupStamp(time.Now())
		var lines []string
		for ref, sha := range before {
			lines = append(lines, "update refs/backup/"+stamp+"/"+ref+" "+sha)
		}
		if err := m.git.UpdateRefs(ctx, repoPath, lines); err != nil {
			return nil, fmt.Errorf("back up hub refs: %w", err)
		}
		res.BackedUp = len(lines)
	}

	// Fetch into a private namespace first; nothing becomes visible until
	// the final transaction below.
	fetched, err := m.git.FetchForImport(ctx, repoPath, rc.url, rc.cred)
	defer m.git.ClearImport(context.Background(), repoPath)
	if err != nil {
		return nil, fmt.Errorf("fetch from %s: %w", rem.Label(), err)
	}
	res.Refs = len(fetched)

	// LFS content before the refs are published, so readers and syncs
	// never see pointers whose objects are still missing.
	if len(fetched) > 0 {
		tips := make([]string, 0, len(fetched))
		for _, sha := range fetched {
			tips = append(tips, sha)
		}
		objs, err := m.git.LFSObjects(ctx, repoPath, tips, nil)
		if err != nil {
			res.Warnings = append(res.Warnings, "scan LFS pointers: "+err.Error())
		} else if len(objs) > 0 {
			if err := m.git.LFSFetchAll(ctx, repoPath, rc.url, rc.cred, fetched); err != nil {
				res.Warnings = append(res.Warnings, "fetch LFS objects: "+firstLine(err.Error()))
			}
			for oid, size := range objs {
				if lfs.Has(repoPath, oid, size) {
					res.LFSObjects++
				}
			}
			if missing := len(objs) - res.LFSObjects; missing > 0 {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%d LFS object(s) referenced by the history could not be fetched", missing))
			}
		}
	}

	// Publish atomically, but only if no push changed the hub meanwhile:
	// every update names the value observed before the fetch.
	var tx []string
	for ref, sha := range fetched {
		tx = append(tx, "update "+ref+" "+sha+" "+orZero(before[ref]))
	}
	if force {
		for ref, sha := range before {
			if _, ok := fetched[ref]; !ok {
				tx = append(tx, "delete "+ref+" "+sha)
			}
		}
	}
	if err := m.git.UpdateRefs(ctx, repoPath, tx); err != nil {
		return nil, fmt.Errorf("%w: the hub repository changed while importing (a push arrived); nothing was replaced — try again", ErrConcurrentChange)
	}
	after := fetched

	// HEAD follows the mirror's default branch.
	head := m.git.RemoteHead(ctx, rc.url, rc.cred)
	if _, ok := after[head]; !ok {
		head = ""
		for _, c := range []string{"refs/heads/main", "refs/heads/master"} {
			if _, ok := after[c]; ok {
				head = c
				break
			}
		}
		if head == "" {
			var branches []string
			for ref := range after {
				if strings.HasPrefix(ref, "refs/heads/") {
					branches = append(branches, ref)
				}
			}
			sort.Strings(branches)
			if len(branches) > 0 {
				head = branches[0]
			}
		}
	}
	if head != "" {
		if err := m.git.SetHead(ctx, repoPath, head); err != nil {
			return nil, err
		}
		res.DefaultBranch = strings.TrimPrefix(head, "refs/heads/")
		repo.DefaultBranch = res.DefaultBranch
		if err := m.store.UpdateRepo(ctx, repo); err != nil {
			return nil, err
		}
	}

	// The source now matches the hub exactly: record it as ours.
	pushed, err := m.store.GetPushedState(ctx, rem.ID)
	if err != nil {
		return nil, err
	}
	var stale []string
	for ref := range pushed {
		if _, ok := after[ref]; !ok {
			stale = append(stale, ref)
		}
	}
	if err := m.store.ApplyPushedState(ctx, rem.ID, after, stale); err != nil {
		return nil, err
	}
	_ = m.store.ReplaceRefState(ctx, rem.ID, after)
	rem.Adopt = true
	if err := m.store.UpdateRemoteConfig(ctx, rem); err != nil {
		return nil, err
	}
	_ = m.store.SetRemoteProvisioned(ctx, rem.ID, true)
	return res, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
