// Package bundles maintains Git bundles of hub repositories for the
// bundle-uri protocol: a client that opts in (transfer.bundleURI=true)
// downloads a pre-built bundle — a static file a CDN can serve — and then
// fetches only what changed since, which makes first clones of large
// repositories much cheaper for the hub.
package bundles

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qwq-inc/gitrepo/internal/gitexec"
)

// Manager creates and finds bundles under Dir/<org>/<name>/.
type Manager struct {
	Dir      string
	Git      *gitexec.Git
	Interval time.Duration // minimum age before a bundle is replaced
	MinSize  int64         // repositories smaller than this get no bundle
	Log      *slog.Logger

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// Bundle is the current bundle of a repository. ID is its creation time in
// Unix seconds, so it doubles as the bundle-uri creationToken.
type Bundle struct {
	ID   string
	Path string
}

var idRe = regexp.MustCompile(`^[0-9]{1,20}$`)

// ValidID reports whether id can name a bundle file.
func ValidID(id string) bool { return idRe.MatchString(id) }

func (m *Manager) dir(slug string) string { return filepath.Join(m.Dir, filepath.FromSlash(slug)) }

// Path is the file of bundle id of slug.
func (m *Manager) Path(slug, id string) string { return filepath.Join(m.dir(slug), id+".bundle") }

func (m *Manager) lock(slug string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks == nil {
		m.locks = map[string]*sync.Mutex{}
	}
	l := m.locks[slug]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[slug] = l
	}
	return l
}

// meta is stored next to each bundle: the repository it belongs to and the
// refs it was built from.
type meta struct {
	repoID      int64
	fingerprint string
}

func readMeta(path string) (meta, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return meta{}, false
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		return meta{}, false
	}
	id, err := strconv.ParseInt(lines[0], 10, 64)
	if err != nil {
		return meta{}, false
	}
	return meta{repoID: id, fingerprint: lines[1]}, true
}

// ids lists bundle ids of slug, newest first.
func (m *Manager) ids(slug string) []string {
	entries, _ := os.ReadDir(m.dir(slug))
	var ids []string
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), ".bundle"); ok && ValidID(id) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, _ := strconv.ParseInt(ids[i], 10, 64)
		b, _ := strconv.ParseInt(ids[j], 10, 64)
		return a > b
	})
	return ids
}

// Current returns the newest bundle that belongs to repository repoID.
func (m *Manager) Current(slug string, repoID int64) (*Bundle, bool) {
	for _, id := range m.ids(slug) {
		if md, ok := readMeta(filepath.Join(m.dir(slug), id+".meta")); ok && md.repoID == repoID {
			return &Bundle{ID: id, Path: m.Path(slug, id)}, true
		}
	}
	return nil, false
}

// Belongs reports whether bundle id of slug was built for repoID.
func (m *Manager) Belongs(slug, id string, repoID int64) bool {
	md, ok := readMeta(filepath.Join(m.dir(slug), id+".meta"))
	return ok && md.repoID == repoID
}

// Remove deletes every bundle of slug: when the repository is unregistered
// or its published history was rewritten (a bundle must never keep offering
// commits that were force-pushed away).
func (m *Manager) Remove(slug string) error {
	l := m.lock(slug)
	l.Lock()
	defer l.Unlock()
	return os.RemoveAll(m.dir(slug))
}

// stale reports whether any bundle of slug has a head that no current
// branch or tag contains, i.e. the history was rewritten since.
func (m *Manager) stale(ctx context.Context, slug, repoPath string) bool {
	for _, id := range m.ids(slug) {
		out, err := m.Git.Run(ctx, "", "bundle", "list-heads", m.Path(slug, id))
		if err != nil {
			return true
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			sha, _, _ := strings.Cut(line, " ")
			if sha == "" {
				continue
			}
			c, err := m.Git.Run(ctx, repoPath, "for-each-ref", "--count=1", "--format=%(refname)", "--contains", sha, "refs/heads/", "refs/tags/")
			if err != nil || len(bytes.TrimSpace(c)) == 0 {
				return true
			}
		}
	}
	return false
}

func (m *Manager) fingerprint(ctx context.Context, repoPath string) (string, int, error) {
	out, err := m.Git.Run(ctx, repoPath, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads/", "refs/tags/")
	if err != nil {
		return "", 0, err
	}
	sum := sha256.Sum256(out)
	return hex.EncodeToString(sum[:]), bytes.Count(out, []byte("\n")), nil
}

func (m *Manager) repoSize(ctx context.Context, repoPath string) (int64, error) {
	out, err := m.Git.Run(ctx, repoPath, "count-objects", "-v")
	if err != nil {
		return 0, err
	}
	var kib int64
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, ": ")
		if ok && (k == "size" || k == "size-pack") {
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			kib += n
		}
	}
	return kib * 1024, nil
}

// Refresh builds a new bundle of the repository's branches and tags when
// the repository is large enough, its refs changed since the current
// bundle, and that bundle is older than Interval (or force is set). It
// returns whether a bundle was written.
func (m *Manager) Refresh(ctx context.Context, slug string, repoID int64, repoPath string, force bool) (bool, error) {
	l := m.lock(slug)
	l.Lock()
	defer l.Unlock()

	if m.stale(ctx, slug, repoPath) {
		os.RemoveAll(m.dir(slug))
		force = true
		if m.Log != nil {
			m.Log.Info("bundles dropped after a history rewrite", "repo", slug)
		}
	}
	fp, nrefs, err := m.fingerprint(ctx, repoPath)
	if err != nil || nrefs == 0 {
		return false, err
	}
	if cur, ok := m.Current(slug, repoID); ok {
		md, _ := readMeta(filepath.Join(m.dir(slug), cur.ID+".meta"))
		created, _ := strconv.ParseInt(cur.ID, 10, 64)
		if md.fingerprint == fp {
			return false, nil
		}
		if !force && time.Since(time.Unix(created, 0)) < m.Interval {
			return false, nil
		}
	}
	if size, err := m.repoSize(ctx, repoPath); err != nil || size < m.MinSize {
		return false, err
	}

	if err := os.MkdirAll(m.dir(slug), 0o755); err != nil {
		return false, err
	}
	id := strconv.FormatInt(time.Now().Unix(), 10)
	if prev := m.ids(slug); len(prev) > 0 && prev[0] >= id && len(prev[0]) == len(id) {
		n, _ := strconv.ParseInt(prev[0], 10, 64)
		id = strconv.FormatInt(n+1, 10) // keep ids strictly increasing
	}
	tmp := filepath.Join(m.dir(slug), "."+id+".tmp")
	defer os.Remove(tmp)
	// Only published refs: hidden refs/backup/* must never be handed out.
	if _, err := m.Git.Run(ctx, repoPath, "bundle", "create", tmp, "--branches", "--tags"); err != nil {
		return false, fmt.Errorf("git bundle create: %w", err)
	}
	if err := os.WriteFile(filepath.Join(m.dir(slug), id+".meta"), []byte(fmt.Sprintf("%d\n%s\n", repoID, fp)), 0o644); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, m.Path(slug, id)); err != nil {
		return false, err
	}
	// Keep the previous bundle for clients that are still downloading it.
	for _, old := range m.ids(slug)[min(2, len(m.ids(slug))):] {
		os.Remove(m.Path(slug, old))
		os.Remove(filepath.Join(m.dir(slug), old+".meta"))
	}
	if m.Log != nil {
		m.Log.Info("bundle created", "repo", slug, "id", id)
	}
	return true, nil
}
