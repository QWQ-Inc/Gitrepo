// Package snapshot backs up and restores a whole Gitrepo instance: the
// database (targets with their encrypted credentials, tokens, keys,
// bindings, mirrors and history), every hub repository including hidden
// refs, Git LFS objects and the SSH host key.
//
// A snapshot is a directory:
//
//	manifest.json             written last; its presence marks a complete snapshot
//	gitrepo.db                consistent SQLite copy (VACUUM INTO)
//	ssh/                      host key and learned mirror host keys
//	repos/<org>/<name>.bundle git bundle --all (absent for empty repositories)
//	repos/<org>/<name>.lfs/   Git LFS objects
//
// Credentials stay encrypted with the instance's secret key, which is not
// part of the snapshot: keep GITREPO_SECRET_KEY separately.
package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qwq-inc/gitrepo/internal/config"
	"github.com/qwq-inc/gitrepo/internal/gitexec"
	"github.com/qwq-inc/gitrepo/internal/lfs"
	"github.com/qwq-inc/gitrepo/internal/registry"
	"github.com/qwq-inc/gitrepo/internal/store"
)

// Format is the snapshot layout version this binary writes and reads.
const Format = 1

type Manifest struct {
	Format    int       `json:"format"`
	CreatedAt time.Time `json:"created_at"`
	Version   string    `json:"gitrepo_version"`
	Repos     []Repo    `json:"repos"`
}

type Repo struct {
	Slug string `json:"slug"`
	// Head is HEAD's symbolic target, e.g. refs/heads/main.
	Head string `json:"head,omitempty"`
	Refs int    `json:"refs"`
	// Bundle is relative to the snapshot; empty for an empty repository.
	Bundle     string `json:"bundle,omitempty"`
	LFSObjects int    `json:"lfs_objects"`
	LFSBytes   int64  `json:"lfs_bytes"`
	// Missing is set when the database lists the repository but its
	// directory did not exist.
	Missing bool `json:"missing,omitempty"`
}

// Create writes a snapshot of the instance described by cfg to dest, which
// must not exist or be empty. It is safe while the server runs: pushes that
// land during the snapshot are either in a repository's bundle or not, never
// half; the database copy is taken first, so every repository it lists is
// included.
func Create(ctx context.Context, cfg *config.Config, g *gitexec.Git, dest, version string, progress io.Writer) (*Manifest, error) {
	if err := emptyDir(dest); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return nil, err
	}
	os.Chmod(dest, 0o700) // holds encrypted credentials and token hashes
	logf := func(format string, args ...any) {
		if progress != nil {
			fmt.Fprintf(progress, format+"\n", args...)
		}
	}

	dbCopy := filepath.Join(dest, "gitrepo.db")
	if err := store.SnapshotTo(cfg.DBPath(), dbCopy); err != nil {
		return nil, err
	}
	st, err := store.Open(dbCopy)
	if err != nil {
		return nil, fmt.Errorf("read database snapshot: %w", err)
	}
	repos, err := st.ListRepos(ctx)
	st.Close()
	if err != nil {
		return nil, err
	}
	logf("database copied: %d repo(s)", len(repos))

	if err := copyFiles(cfg.SSHDir(), filepath.Join(dest, "ssh"), false); err != nil {
		return nil, fmt.Errorf("copy ssh keys: %w", err)
	}

	m := &Manifest{Format: Format, CreatedAt: time.Now().UTC(), Version: version}
	for _, r := range repos {
		entry, err := backupRepo(ctx, g, filepath.Join(cfg.ReposDir(), r.Slug+".git"), dest, r.Slug)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", r.Slug, err)
		}
		switch {
		case entry.Missing:
			logf("%s: missing on disk, skipped", r.Slug)
		case entry.Bundle == "":
			logf("%s: empty", r.Slug)
		default:
			logf("%s: %d refs, %d LFS objects", r.Slug, entry.Refs, entry.LFSObjects)
		}
		m.Repos = append(m.Repos, *entry)
	}
	if err := writeJSON(filepath.Join(dest, "manifest.json"), m); err != nil {
		return nil, err
	}
	return m, nil
}

func backupRepo(ctx context.Context, g *gitexec.Git, repoPath, dest, slug string) (*Repo, error) {
	e := &Repo{Slug: slug}
	if _, err := os.Stat(filepath.Join(repoPath, "HEAD")); err != nil {
		e.Missing = true
		return e, nil
	}
	if head, err := g.SymbolicRef(ctx, repoPath); err == nil {
		e.Head = head
	}
	base := filepath.Join(dest, "repos", filepath.FromSlash(slug))
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		return nil, err
	}
	refs, err := bundleRepo(ctx, g, repoPath, base+".bundle")
	if errors.Is(err, fs.ErrNotExist) {
		e.Missing = true // deleted while the snapshot was taken
		return e, nil
	}
	if err != nil {
		return nil, err
	}
	// The count comes from the bundle itself, not from a separate listing
	// that a concurrent push could make disagree with it.
	e.Refs = refs
	if refs > 0 {
		e.Bundle = filepath.ToSlash(filepath.Join("repos", slug+".bundle"))
	}
	// LFS objects are copied after the bundle: the hub only accepts a push
	// once its LFS objects are stored, so everything the bundle references
	// is already on disk.
	n, size, err := copyLFS(repoPath, base+".lfs")
	if err != nil {
		return nil, fmt.Errorf("copy LFS objects: %w", err)
	}
	e.LFSObjects, e.LFSBytes = n, size
	return e, nil
}

// bundleRepo writes `git bundle --all` of the repository and returns the
// number of refs in it (0, and no file, for an empty repository). The
// bundle records the refs it read and packs exactly what they reach, so a
// push during the backup cannot make it inconsistent. A concurrent purge can
// remove objects of refs it already listed; that attempt fails and is
// retried.
func bundleRepo(ctx context.Context, g *gitexec.Git, repoPath, file string) (int, error) {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if _, statErr := os.Stat(filepath.Join(repoPath, "HEAD")); statErr != nil {
			return 0, fs.ErrNotExist
		}
		var refs map[string]string
		if refs, err = g.ForEachRef(ctx, repoPath); err != nil {
			continue
		}
		if len(refs) == 0 {
			return 0, nil
		}
		os.Remove(file)
		if _, err = g.Run(ctx, repoPath, "bundle", "create", "--quiet", file, "--all"); err != nil {
			continue
		}
		var out []byte
		if out, err = g.Run(ctx, repoPath, "bundle", "list-heads", file); err != nil {
			continue
		}
		n := 0
		for _, line := range strings.Split(string(out), "\n") {
			if _, ref, ok := strings.Cut(strings.TrimSpace(line), " "); ok && strings.HasPrefix(ref, "refs/") {
				n++
			}
		}
		return n, nil
	}
	if _, statErr := os.Stat(filepath.Join(repoPath, "HEAD")); statErr != nil {
		return 0, fs.ErrNotExist
	}
	return 0, fmt.Errorf("git bundle: %w", err)
}

// Restore recreates an instance from the snapshot at src in cfg's data
// directory, which must not have a database or repositories yet. The
// server must not be running; start it afterwards with the same secret key.
func Restore(ctx context.Context, cfg *config.Config, g *gitexec.Git, src string, progress io.Writer) (*Manifest, error) {
	logf := func(format string, args ...any) {
		if progress != nil {
			fmt.Fprintf(progress, format+"\n", args...)
		}
	}
	m, err := ReadManifest(src)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(cfg.DBPath()); err == nil {
		return nil, fmt.Errorf("%s already exists: restore only into a new data directory", cfg.DBPath())
	}
	if err := emptyDir(cfg.ReposDir()); err != nil {
		return nil, fmt.Errorf("%w: restore only into a new data directory", err)
	}
	for _, d := range []string{cfg.DataDir, cfg.ReposDir()} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, err
		}
	}

	// Repositories first: if anything fails, no database exists yet and the
	// half-restored directory is easy to recognise and remove.
	for _, r := range m.Repos {
		if r.Missing {
			// Deleted while the snapshot was taken: keep the database and
			// the disk consistent; `gr repo import` can refill it.
			if err := g.InitBare(ctx, filepath.Join(cfg.ReposDir(), r.Slug+".git"), "main"); err != nil {
				return nil, fmt.Errorf("%s: %w", r.Slug, err)
			}
			logf("%s: was missing when the snapshot was taken; restored empty (use gr repo import to refill it from a mirror)", r.Slug)
			continue
		}
		if err := restoreRepo(ctx, g, src, filepath.Join(cfg.ReposDir(), r.Slug+".git"), r); err != nil {
			return nil, fmt.Errorf("%s: %w", r.Slug, err)
		}
		logf("%s: %d refs, %d LFS objects", r.Slug, r.Refs, r.LFSObjects)
	}
	if err := copyFiles(filepath.Join(src, "ssh"), cfg.SSHDir(), false); err != nil {
		return nil, fmt.Errorf("restore ssh keys: %w", err)
	}
	tmp := cfg.DBPath() + ".restore"
	if err := copyFile(filepath.Join(src, "gitrepo.db"), tmp, 0o600); err != nil {
		return nil, err
	}
	// Opening applies migrations if the snapshot is from an older version.
	st, err := store.Open(tmp)
	if err != nil {
		os.Remove(tmp)
		return nil, fmt.Errorf("database snapshot: %w", err)
	}
	// Leases held by the original instance's syncs mean nothing here and
	// would block syncs after the first start.
	err = st.ClearLeases(ctx)
	st.Close()
	if err != nil {
		os.Remove(tmp)
		return nil, err
	}
	if err := os.Rename(tmp, cfg.DBPath()); err != nil {
		return nil, err
	}
	logf("database restored")
	return m, nil
}

func restoreRepo(ctx context.Context, g *gitexec.Git, src, repoPath string, r Repo) error {
	if err := g.InitBare(ctx, repoPath, "main"); err != nil {
		return err
	}
	if r.Bundle != "" {
		bundle := filepath.Join(src, filepath.FromSlash(r.Bundle))
		if _, err := g.Run(ctx, repoPath, "bundle", "verify", "--quiet", bundle); err != nil {
			return fmt.Errorf("bundle %s is damaged: %w", r.Bundle, err)
		}
		if _, err := g.Run(ctx, repoPath, "fetch", "--quiet", "--no-write-fetch-head", bundle, "+refs/*:refs/*"); err != nil {
			return fmt.Errorf("unpack bundle: %w", err)
		}
		refs, err := g.ForEachRef(ctx, repoPath)
		if err != nil {
			return err
		}
		if len(refs) != r.Refs {
			return fmt.Errorf("restored %d refs, the snapshot lists %d", len(refs), r.Refs)
		}
	}
	if r.Head != "" {
		if err := g.SetHead(ctx, repoPath, r.Head); err != nil {
			return err
		}
	}
	base := strings.TrimSuffix(filepath.Join(src, filepath.FromSlash(r.Bundle)), ".bundle")
	if r.Bundle == "" {
		base = filepath.Join(src, "repos", filepath.FromSlash(r.Slug))
	}
	n, _, err := copyLFSBack(base+".lfs", repoPath)
	if err != nil {
		return fmt.Errorf("restore LFS objects: %w", err)
	}
	if n != r.LFSObjects {
		return fmt.Errorf("restored %d LFS objects, the snapshot lists %d", n, r.LFSObjects)
	}
	return nil
}

// ReadManifest reads and checks a snapshot's manifest.
func ReadManifest(src string) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(src, "manifest.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s is not a complete snapshot (no manifest.json)", src)
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest.json: %w", err)
	}
	if m.Format != Format {
		return nil, fmt.Errorf("snapshot format %d is not supported by this version (%d)", m.Format, Format)
	}
	for _, r := range m.Repos {
		// The slugs come from a file: never let one point outside the
		// data directory.
		if org, name, err := registry.ParseSlug(r.Slug); err != nil || org+"/"+name != r.Slug {
			return nil, fmt.Errorf("manifest.json: invalid repository %q", r.Slug)
		}
		if r.Bundle != "" && r.Bundle != "repos/"+r.Slug+".bundle" {
			return nil, fmt.Errorf("manifest.json: unexpected bundle path %q", r.Bundle)
		}
	}
	return &m, nil
}

// ------------------------------------------------------------------ files

func emptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s is not empty", dir)
	}
	return nil
}

// copyLFS copies (hard-links where possible) a repository's LFS objects.
// Objects are immutable once stored, so sharing the inode is safe.
func copyLFS(repoPath, dest string) (int, int64, error) {
	root := filepath.Join(repoPath, "lfs", "objects")
	n, size := 0, int64(0)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return filepath.SkipDir
		}
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() || !lfs.ValidOID(d.Name()) {
			return nil
		}
		oid := d.Name()
		target := filepath.Join(dest, oid[0:2], oid[2:4], oid)
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil // removed by a purge meanwhile: no longer referenced
		}
		if err != nil {
			return err
		}
		if err := linkOrCopy(path, target); errors.Is(err, fs.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		n++
		size += info.Size()
		return nil
	})
	return n, size, err
}

func copyLFSBack(src, repoPath string) (int, int64, error) {
	if _, err := os.Stat(src); errors.Is(err, fs.ErrNotExist) {
		return 0, 0, nil
	}
	n, size := 0, int64(0)
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !lfs.ValidOID(d.Name()) {
			return nil
		}
		// Only regular files whose content matches their name are taken:
		// a snapshot is data from outside, and a symlink here would make
		// the LFS endpoint serve any file on the server.
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		written, err := copyVerified(path, lfs.ObjectPath(repoPath, d.Name()), d.Name())
		if err != nil {
			return err
		}
		n++
		size += written
		return nil
	})
	return n, size, err
}

// copyVerified copies an LFS object and checks its SHA-256 against oid.
func copyVerified(src, dst, oid string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return 0, err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return 0, err
	}
	h := sha256.New()
	written, err := io.Copy(io.MultiWriter(out, h), in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != oid {
		err = fmt.Errorf("%s: content does not match its object id", src)
	}
	if err != nil {
		os.Remove(dst)
		return 0, err
	}
	return written, nil
}

func linkOrCopy(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return copyFile(src, dst, 0o640)
}

// copyFiles copies the regular files directly in src (not subdirectories).
func copyFiles(src, dst string, mustExist bool) error {
	entries, err := os.ReadDir(src)
	if errors.Is(err, fs.ErrNotExist) && !mustExist {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	names := []string{}
	for _, e := range entries {
		if e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if err := copyFile(filepath.Join(src, n), filepath.Join(dst, n), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
