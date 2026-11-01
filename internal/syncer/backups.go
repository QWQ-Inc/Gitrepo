package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qwq-inc/gitrepo/internal/gitexec"
	"github.com/qwq-inc/gitrepo/internal/lfs"
	"github.com/qwq-inc/gitrepo/internal/store"
)

// Backup is one hidden refs/backup/* entry of a hub repository.
type Backup struct {
	// Ref is the full backup ref name; it identifies the backup.
	Ref string `json:"ref"`
	// Source is "hub" (overwritten on the hub by a push or import) or
	// "remote" (a mirror's own commits, saved before an overwrite).
	Source   string `json:"source"`
	RemoteID int64  `json:"remote_id,omitempty"`
	// Original is the branch or tag the content was on.
	Original  string    `json:"original"`
	SHA       string    `json:"sha"`
	Subject   string    `json:"subject,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ErrNoSuchBackup is returned for unknown backup refs.
var ErrNoSuchBackup = errors.New("no such backup")

func parseBackupRef(ref string) (b Backup, ok bool) {
	rest, ok := strings.CutPrefix(ref, "refs/backup/")
	if !ok {
		return b, false
	}
	stamp, orig, ok := strings.Cut(rest, "/")
	if !ok {
		return b, false
	}
	ts, err := time.Parse(stampLayout, stamp)
	if err != nil {
		return b, false
	}
	b = Backup{Ref: ref, Source: "hub", Original: orig, CreatedAt: ts}
	if r, ok := strings.CutPrefix(orig, "remotes/"); ok {
		idStr, o, ok := strings.Cut(r, "/")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if ok && err == nil {
			b.Source, b.RemoteID, b.Original = "remote", id, o
		}
	}
	return b, true
}

// ListBackups returns the repository's backups, newest first.
func ListBackups(ctx context.Context, g *gitexec.Git, repoPath string, retention time.Duration) ([]Backup, error) {
	out, err := g.Run(ctx, repoPath, "for-each-ref", "--format=%(objectname) %(refname) %(contents:subject)", "refs/backup/")
	if err != nil {
		return nil, err
	}
	var list []Backup
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.SplitN(line, " ", 3)
		if len(f) < 2 {
			continue
		}
		b, ok := parseBackupRef(f[1])
		if !ok {
			continue
		}
		b.SHA = f[0]
		if len(f) == 3 {
			b.Subject = f[2]
		}
		b.ExpiresAt = b.CreatedAt.Add(retention)
		list = append(list, b)
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].CreatedAt.After(list[j].CreatedAt)
		}
		return list[i].Ref < list[j].Ref
	})
	return list, nil
}

// RestoreBackup publishes a backup as a new branch (which must not exist)
// and queues a sync so the mirrors receive it. It returns the commit.
func (m *Manager) RestoreBackup(ctx context.Context, repo *store.Repo, backupRef, branch string) (string, error) {
	if _, ok := parseBackupRef(backupRef); !ok {
		return "", ErrNoSuchBackup
	}
	repoPath := m.reg.Path(repo.Slug)
	refs, err := m.git.ForEachRef(ctx, repoPath, backupRef)
	if err != nil {
		return "", err
	}
	obj, ok := refs[backupRef]
	if !ok {
		return "", ErrNoSuchBackup
	}
	if _, err := m.git.Run(ctx, "", "check-ref-format", "--branch", branch); err != nil || strings.HasPrefix(branch, "-") {
		return "", fmt.Errorf("invalid branch name %q", branch)
	}
	// A backed-up annotated tag points at a tag object; a branch needs the
	// commit it tags.
	out, err := m.git.Run(ctx, repoPath, "rev-parse", "--verify", "--quiet", obj+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("backup %s does not point to a commit", backupRef)
	}
	sha := strings.TrimSpace(string(out))
	target := "refs/heads/" + branch
	if existing, err := m.git.ForEachRef(ctx, repoPath, target); err == nil {
		if _, taken := existing[target]; taken {
			return "", fmt.Errorf("branch %s already exists", branch)
		}
	}
	if err := m.git.UpdateRefs(ctx, repoPath, []string{"create " + target + " " + sha}); err != nil {
		return "", fmt.Errorf("create branch %s: %w", branch, err)
	}
	m.Enqueue(repo.ID, Request{Trigger: TriggerManual})
	return sha, nil
}

// PurgeResult reports what a purge removed.
type PurgeResult struct {
	Backups    int `json:"backups"`
	LFSObjects int `json:"lfs_objects"`
}

// PurgeBackups deletes backups (all of them when refs is empty) and then
// removes the objects only they referenced from the hub repository, so
// purged commits can no longer be fetched even by SHA. With purgeLFS, LFS
// objects no ref references any more are deleted too.
//
// It holds the repository's sync lease (no sync or import runs) and, through
// Options.Quiesce, keeps pushes out while it runs. Objects younger than five
// minutes are kept as a further margin, e.g. LFS uploads for a push that has
// not started yet.
func (m *Manager) PurgeBackups(ctx context.Context, repo *store.Repo, refs []string, purgeLFS bool) (*PurgeResult, error) {
	release, err := m.holdLease(ctx, repo.ID, "purge")
	if err != nil {
		return nil, err
	}
	defer release()
	// No push may run while unreachable objects are dropped: a push can
	// make an old object (or LFS object) reachable again after gc or the
	// LFS walk has decided it is garbage.
	if m.opts.Quiesce != nil {
		qctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		reopen, err := m.opts.Quiesce(qctx, repo.ID)
		cancel()
		if err != nil {
			return nil, err
		}
		defer reopen()
	}
	repoPath := m.reg.Path(repo.Slug)
	existing, err := m.git.ForEachRef(ctx, repoPath, "refs/backup/")
	if err != nil {
		return nil, err
	}
	var lines []string
	if len(refs) == 0 {
		for ref := range existing {
			lines = append(lines, "delete "+ref)
		}
	} else {
		for _, ref := range refs {
			if _, ok := existing[ref]; !ok {
				return nil, fmt.Errorf("%w: %s", ErrNoSuchBackup, ref)
			}
			lines = append(lines, "delete "+ref)
		}
	}
	if err := m.git.UpdateRefs(ctx, repoPath, lines); err != nil {
		return nil, err
	}
	res := &PurgeResult{Backups: len(lines)}
	if _, err := m.git.Run(ctx, repoPath, "-c", "gc.reflogExpire=now", "-c", "gc.reflogExpireUnreachable=now",
		"gc", "--quiet", "--prune=5.minutes.ago"); err != nil {
		return res, fmt.Errorf("git gc: %w", err)
	}
	if purgeLFS {
		n, err := m.pruneLFS(ctx, repoPath)
		res.LFSObjects = n
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

// pruneLFS deletes LFS objects that no ref of the repository references.
func (m *Manager) pruneLFS(ctx context.Context, repoPath string) (int, error) {
	all, err := m.git.ForEachRef(ctx, repoPath)
	if err != nil {
		return 0, err
	}
	tips := make([]string, 0, len(all))
	for _, sha := range all {
		tips = append(tips, sha)
	}
	keep := map[string]int64{}
	if len(tips) > 0 {
		if keep, err = m.git.LFSObjects(ctx, repoPath, tips, nil); err != nil {
			return 0, err
		}
	}
	removed := 0
	cutoff := time.Now().Add(-5 * time.Minute)
	root := filepath.Join(repoPath, "lfs", "objects")
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		oid := d.Name()
		if !lfs.ValidOID(oid) {
			return nil
		}
		if _, ok := keep[oid]; ok {
			return nil
		}
		if fi, err := d.Info(); err != nil || fi.ModTime().After(cutoff) {
			return nil // possibly uploaded for a push that is still in flight
		}
		if os.Remove(path) == nil {
			removed++
		}
		return nil
	})
	return removed, err
}

// holdLease takes the repository's sync lease for a maintenance task,
// waiting up to two minutes for a running sync. Like a sync run, it holds a
// short lease and renews it until released, so a long task (gc of a large
// repository) keeps it and a crashed process blocks the repository for at
// most leaseTTL.
func (m *Manager) holdLease(ctx context.Context, repoID int64, what string) (func(), error) {
	name := "repo:" + strconv.FormatInt(repoID, 10)
	holder := m.holder + "-" + what
	deadline := time.Now().Add(2 * time.Minute)
	for {
		ok, err := m.store.AcquireLease(ctx, name, holder, leaseTTL.Milliseconds())
		if err != nil {
			return nil, err
		}
		if ok {
			stop := make(chan struct{})
			go func() {
				t := time.NewTicker(leaseTTL / 3)
				defer t.Stop()
				for {
					select {
					case <-stop:
						return
					case <-t.C:
						if _, err := m.store.AcquireLease(context.Background(), name, holder, leaseTTL.Milliseconds()); err != nil {
							m.log.Warn("renew lease", "lease", name, "task", what, "err", err)
						}
					}
				}
			}()
			var once sync.Once
			return func() {
				once.Do(func() {
					close(stop)
					m.store.ReleaseLease(context.Background(), name, holder)
				})
			}, nil
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
}
