package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qwq-inc/gitrepo/internal/gitexec"
	"github.com/qwq-inc/gitrepo/internal/lfs"
	"github.com/qwq-inc/gitrepo/internal/provider"
	"github.com/qwq-inc/gitrepo/internal/store"
)

// A write check (gr target check NAME --write) does on the platform what a
// sync and a release do, in a scratch repository it creates and deletes
// afterwards: create the repository, push a branch and a tag (and an LFS
// object), set the default branch and the visibility, publish a release
// with a file and remove it. It proves that the credential has every
// permission Gitrepo needs - and that the platform behaves as the adapter
// expects - before real repositories depend on it.

type checkStep struct {
	Name   string `json:"name"`
	State  string `json:"state"` // ok, failed, skipped
	Detail string `json:"detail,omitempty"`
}

type writeCheckResult struct {
	Steps []checkStep `json:"steps"`
	// Leftover names the scratch repository when it could not be deleted.
	Leftover string `json:"leftover,omitempty"`
}

func (r *writeCheckResult) add(name, state, format string, args ...any) {
	r.Steps = append(r.Steps, checkStep{Name: name, State: state, Detail: fmt.Sprintf(format, args...)})
}

func (r *writeCheckResult) ok() bool {
	for _, s := range r.Steps {
		if s.State == "failed" {
			return false
		}
	}
	return true
}

const checkTag = "gitrepo-check"

// writeCheck runs the write check for t (secret is the resolved credential:
// for a GitHub App an installation token).
func (s *Server) writeCheck(ctx context.Context, t *store.Target, secret string) *writeCheckResult {
	res := &writeCheckResult{}
	p, err := provider.Get(t.Provider)
	if err != nil {
		res.add("provider", "failed", "%v", err)
		return res
	}
	if !p.Caps().CreateRepo {
		res.add("create repository", "skipped", "%s targets have no API to create a repository; syncing to an existing one shows whether pushes work", t.Provider)
		return res
	}
	b := make([]byte, 4)
	rand.Read(b)
	spec := provider.RepoSpec{Name: "gitrepo-check-" + hex.EncodeToString(b), Visibility: "private"}
	label := spec.Name
	if t.Namespace != "" {
		label = t.Namespace + "/" + spec.Name
	}
	created, err := p.EnsureRepo(ctx, t, secret, spec)
	switch {
	case err != nil:
		res.add("create repository", "failed", "%v", err)
		return res
	case !created:
		res.add("create repository", "failed", "%s already exists", label)
		return res
	}
	res.add("create repository", "ok", "%s (private)", label)
	res.Leftover = label
	defer func() {
		del, ok := p.(provider.RepoDeleter)
		if !ok {
			res.add("delete repository", "skipped", "%s cannot delete repositories; delete %s by hand", t.Provider, label)
			return
		}
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if err := del.DeleteRepo(dctx, t, secret, spec); err != nil {
			res.add("delete repository", "failed", "%v; delete %s by hand (deleting needs more permission than mirroring: it is not required for Gitrepo to work)", err, label)
			return
		}
		res.add("delete repository", "ok", "%s", label)
		res.Leftover = ""
	}()

	url, err := p.RepoURL(t, spec.Namespace, spec.Name)
	if err != nil {
		res.add("push", "failed", "%v", err)
		return res
	}
	cred := p.GitCred(t, secret)
	dir, err := os.MkdirTemp(s.cfg.TmpDir(), "write-check-")
	if err != nil {
		res.add("push", "failed", "%v", err)
		return res
	}
	defer os.RemoveAll(dir)
	repo := filepath.Join(dir, "check.git")
	commit, tagObj, oid, err := s.scratchContent(ctx, repo)
	if err != nil {
		res.add("push", "failed", "prepare content: %v", err)
		return res
	}

	// LFS objects go first, as in a sync.
	if s.git.LFSAvailable(ctx) {
		if err := s.git.LFSPush(ctx, repo, url, cred, []string{oid}); err != nil {
			res.add("Git LFS upload", "failed", "%v (is Git LFS enabled on the server?)", err)
		} else {
			res.add("Git LFS upload", "ok", "1 object")
		}
	} else {
		res.add("Git LFS upload", "skipped", "git-lfs is not installed on this host")
	}

	pr, err := s.git.Push(ctx, repo, url, cred, []gitexec.PushOp{
		{Ref: "refs/heads/main", New: commit},
		{Ref: "refs/tags/" + checkTag, New: tagObj},
	}, false)
	if err == nil {
		for ref, st := range pr.Refs {
			if !st.OK() {
				err = fmt.Errorf("%s: %s %s", ref, st.Summary, st.Reason)
			}
		}
	}
	if err != nil {
		res.add("push", "failed", "%v", err)
		return res
	}
	refs, err := s.git.LsRemote(ctx, url, cred)
	if err != nil || refs["refs/heads/main"] != commit || refs["refs/tags/"+checkTag] != tagObj {
		res.add("push", "failed", "the pushed refs are not on the platform (ls-remote: %v %v)", refs, err)
		return res
	}
	res.add("push", "ok", "branch main and tag %s", checkTag)

	if p.Caps().DefaultBranch {
		if err := p.SetDefaultBranch(ctx, t, secret, spec, "main"); err != nil {
			res.add("default branch", "failed", "%v", err)
		} else {
			res.add("default branch", "ok", "main")
		}
	}
	if vs, ok := p.(provider.VisibilitySetter); ok {
		if err := vs.SetVisibility(ctx, t, secret, spec, "private"); err != nil {
			res.add("visibility", "failed", "%v", err)
		} else {
			res.add("visibility", "ok", "private")
		}
	}
	if rp, ok := p.(provider.Releaser); ok {
		s.checkRelease(ctx, res, rp, t, secret, spec, commit)
	}
	return res
}

// checkRelease creates a release with a file, reads it back and removes both.
func (s *Server) checkRelease(ctx context.Context, res *writeCheckResult, rp provider.Releaser, t *store.Target, secret string, spec provider.RepoSpec, commit string) {
	rr, err := rp.CreateRelease(ctx, t, secret, spec, provider.Release{
		Tag: checkTag, Name: "Gitrepo write check", Body: "Created by Gitrepo's write check; it is deleted again right away.", Commit: commit,
	})
	if err != nil {
		res.add("release", "failed", "create: %v", err)
		return
	}
	content := []byte("gitrepo write check\n")
	ra, err := rp.UploadAsset(ctx, t, secret, spec, rr, provider.Asset{
		Name: "check.txt", ContentType: "text/plain", Size: int64(len(content)),
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(content)), nil },
	})
	if err != nil {
		res.add("release", "failed", "upload a file: %v", err)
	} else if got, err := rp.GetRelease(ctx, t, secret, spec, checkTag); err != nil || got == nil {
		res.add("release", "failed", "read it back: %v", err)
	} else {
		found := false
		for _, a := range got.Assets {
			found = found || a.ID == ra.ID
		}
		if !found {
			res.add("release", "failed", "the uploaded file is not listed on the release")
		} else if err := rp.DeleteAsset(ctx, t, secret, spec, got, ra.ID); err != nil {
			res.add("release", "failed", "delete the file: %v", err)
		} else {
			res.add("release", "ok", "created with a file, read back, file deleted")
		}
	}
	if err := rp.DeleteRelease(ctx, t, secret, spec, rr); err != nil {
		res.add("release (delete)", "failed", "%v", err)
	}
}

// scratchContent makes a bare repository with one commit (a README), an
// annotated tag on it and one LFS object in its store.
func (s *Server) scratchContent(ctx context.Context, repo string) (commit, tagObj, oid string, err error) {
	if err = s.git.InitBare(ctx, repo, "main"); err != nil {
		return
	}
	id := []string{"-c", "user.name=Gitrepo", "-c", "user.email=gitrepo@localhost"}
	run := func(stdin string, args ...string) (string, error) {
		out, err := s.git.RunInput(ctx, repo, []byte(stdin), append(id, args...)...)
		return strings.TrimSpace(string(out)), err
	}
	blob, err := run("This repository was created by Gitrepo's write check (gr target check --write) and can be deleted.\n", "hash-object", "-w", "--stdin")
	if err != nil {
		return
	}
	tree, err := run("100644 blob "+blob+"\tREADME.md\n", "mktree")
	if err != nil {
		return
	}
	if commit, err = run("", "commit-tree", tree, "-m", "Gitrepo write check"); err != nil {
		return
	}
	if _, err = run("", "tag", "-a", checkTag, "-m", "Gitrepo write check", commit); err != nil {
		return
	}
	if tagObj, err = run("", "rev-parse", "refs/tags/"+checkTag); err != nil {
		return
	}
	// An LFS object (content unique per check, so it is really uploaded).
	content := []byte("gitrepo write check " + time.Now().UTC().Format(time.RFC3339Nano) + "\n")
	sum := sha256.Sum256(content)
	oid = hex.EncodeToString(sum[:])
	path := lfs.ObjectPath(repo, oid)
	if err = os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		err = os.WriteFile(path, content, 0o644)
	}
	return
}
