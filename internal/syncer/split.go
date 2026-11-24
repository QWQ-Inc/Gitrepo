package syncer

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/qwq-inc/gitrepo/internal/gitexec"
)

// Subdirectory splits (View.Subdir).
//
// The split of a hub commit c is a commit whose tree is c's tree at the
// subdirectory, whose parents are the splits of c's parents, and whose
// author, committer and message are c's. Commits that do not change the
// subdirectory map to their parent's split, so the split history holds
// only the commits that touched it. Merge parents that are ancestors of
// another parent are dropped (a merge of a branch that never touched the
// directory is no merge there). The result is deterministic: the same hub
// history always gives the same split commits.
//
// Results are cached per remote (<repo>.git/gitrepo/split/<remote>.cache),
// so a sync only processes new commits, and the split values are kept
// reachable under refs/gitrepo/split/<remote>/ (hidden from clients), so
// gc never prunes what a mirror was sent.

const splitCacheHeader = "gitrepo-split 1"

type splitInfo struct {
	tree    string
	gen     int // 1 + the largest parent generation
	parents []string
}

type splitCache struct {
	prefix  string
	commits map[string]string // hub commit → split commit ("" = nothing in the subdirectory)
	info    map[string]*splitInfo
}

func newSplitCache(prefix string) *splitCache {
	return &splitCache{prefix: prefix, commits: map[string]string{}, info: map[string]*splitInfo{}}
}

func (m *Manager) splitCachePath(repoPath string, remoteID int64) string {
	return filepath.Join(repoPath, "gitrepo", "split", strconv.FormatInt(remoteID, 10)+".cache")
}

func splitRefPrefix(remoteID int64) string {
	return "refs/gitrepo/split/" + strconv.FormatInt(remoteID, 10) + "/"
}

func loadSplitCache(path, prefix string) *splitCache {
	c := newSplitCache(prefix)
	f, err := os.Open(path)
	if err != nil {
		return c
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	if !sc.Scan() || sc.Text() != splitCacheHeader || !sc.Scan() || sc.Text() != "prefix "+prefix {
		return c // another version or another directory: start over
	}
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		switch {
		case len(f) == 3 && f[0] == "C":
			if f[2] == "-" {
				f[2] = ""
			}
			c.commits[f[1]] = f[2]
		case len(f) >= 4 && f[0] == "S":
			gen, err := strconv.Atoi(f[3])
			if err != nil {
				return newSplitCache(prefix)
			}
			c.info[f[1]] = &splitInfo{tree: f[2], gen: gen, parents: append([]string(nil), f[4:]...)}
		default:
			return newSplitCache(prefix)
		}
	}
	if sc.Err() != nil {
		return newSplitCache(prefix)
	}
	return c
}

func (c *splitCache) save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b bytes.Buffer
	b.WriteString(splitCacheHeader + "\nprefix " + c.prefix + "\n")
	keys := make([]string, 0, len(c.commits))
	for k := range c.commits {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := c.commits[k]
		if v == "" {
			v = "-"
		}
		b.WriteString("C " + k + " " + v + "\n")
	}
	keys = keys[:0]
	for k := range c.info {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		in := c.info[k]
		b.WriteString("S " + k + " " + in.tree + " " + strconv.Itoa(in.gen))
		for _, p := range in.parents {
			b.WriteString(" " + p)
		}
		b.WriteString("\n")
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// isAncestor reports whether split commit a is an ancestor of b.
func (c *splitCache) isAncestor(a, b string) bool {
	if a == b {
		return true
	}
	ia := c.info[a]
	if ia == nil {
		return false
	}
	seen := map[string]bool{}
	stack := []string{b}
	for len(stack) > 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		ix := c.info[x]
		if ix == nil {
			continue
		}
		for _, p := range ix.parents {
			if p == a {
				return true
			}
			if ip := c.info[p]; ip != nil && ip.gen > ia.gen && !seen[p] {
				seen[p] = true
				stack = append(stack, p)
			}
		}
	}
	return false
}

// splitLock serializes splits of one remote.
func (m *Manager) splitLock(remoteID int64) *sync.Mutex {
	m.splitMu.Lock()
	defer m.splitMu.Unlock()
	if m.splitLocks == nil {
		m.splitLocks = map[int64]*sync.Mutex{}
	}
	l := m.splitLocks[remoteID]
	if l == nil {
		l = &sync.Mutex{}
		m.splitLocks[remoteID] = l
	}
	return l
}

var errSplitCache = errors.New("split cache is inconsistent")

// splitRefs returns what the refs in want (mirror ref → hub value) are in
// the history of prefix: split commits, and for annotated tags new tag
// objects (named after the mirror tag) pointing at them. Refs whose commit
// has nothing in prefix are left out.
func (m *Manager) splitRefs(ctx context.Context, repoPath string, remoteID int64, prefix string, want map[string]string) (map[string]string, error) {
	l := m.splitLock(remoteID)
	l.Lock()
	defer l.Unlock()
	path := m.splitCachePath(repoPath, remoteID)
	cache := loadSplitCache(path, prefix)
	out, err := m.splitWith(ctx, repoPath, remoteID, cache, path, want)
	if errors.Is(err, errSplitCache) {
		// Rebuild from scratch: the result is the same, only slower.
		m.log.Warn("rebuilding the split cache", "repo", repoPath, "remote", remoteID)
		out, err = m.splitWith(ctx, repoPath, remoteID, newSplitCache(prefix), path, want)
	}
	return out, err
}

type peeled struct {
	commit string
	tag    []byte // the outermost annotated tag object, if any
}

func (m *Manager) splitWith(ctx context.Context, repoPath string, remoteID int64, cache *splitCache, cachePath string, want map[string]string) (map[string]string, error) {
	format, err := m.git.ObjectFormat(ctx, repoPath)
	if err != nil {
		return nil, err
	}
	cf, err := m.git.OpenCatFile(ctx, repoPath)
	if err != nil {
		return nil, err
	}
	defer cf.Close()

	// What each ref points at.
	targets := map[string]peeled{}
	var tips []string
	seenTip := map[string]bool{}
	for ref, val := range want {
		p, ok, err := peelToCommit(cf, val)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // a tag of a tree or blob: nothing to split
		}
		targets[ref] = p
		if !seenTip[p.commit] {
			seenTip[p.commit] = true
			tips = append(tips, p.commit)
		}
	}
	sort.Strings(tips)

	walked := false
	var reachable map[string]bool
	allCached := true
	for _, t := range tips {
		if _, ok := cache.commits[t]; !ok {
			allCached = false
		}
	}
	var newCommits [][]byte
	var newIDs []string
	if !allCached {
		walked = true
		args := append([]string{"rev-list", "--reverse", "--topo-order", "--parents"}, tips...)
		raw, err := m.git.Run(ctx, repoPath, args...)
		if err != nil {
			return nil, fmt.Errorf("list history: %w", err)
		}
		reachable = map[string]bool{}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			f := strings.Fields(line)
			if len(f) == 0 {
				continue
			}
			c, parents := f[0], f[1:]
			reachable[c] = true
			if _, ok := cache.commits[c]; ok {
				continue
			}
			split, content, err := splitOne(cf, cache, format, c, parents)
			if err != nil {
				return nil, err
			}
			cache.commits[c] = split
			if content != nil {
				newCommits = append(newCommits, content)
				newIDs = append(newIDs, split)
			}
		}
	}

	// The refs' values, with annotated tags rewritten.
	out := map[string]string{}
	var tagObjs [][]byte
	var tagIDs []string
	for ref, p := range targets {
		split := cache.commits[p.commit]
		if split == "" {
			continue
		}
		if p.tag == nil {
			out[ref] = split
			continue
		}
		content := rewriteTag(p.tag, split, strings.TrimPrefix(ref, "refs/tags/"))
		id := gitexec.HashObject(format, "tag", content)
		tagObjs = append(tagObjs, content)
		tagIDs = append(tagIDs, id)
		out[ref] = id
	}

	// Store the new objects, then make the values reachable.
	for _, batch := range []struct {
		typ      string
		contents [][]byte
		ids      []string
	}{{"commit", newCommits, newIDs}, {"tag", tagObjs, tagIDs}} {
		got, err := m.git.WriteObjects(ctx, repoPath, batch.typ, batch.contents)
		if err != nil {
			return nil, fmt.Errorf("write split objects: %w", err)
		}
		for i := range got {
			if got[i] != batch.ids[i] {
				return nil, fmt.Errorf("write split objects: git computed %s, expected %s", got[i], batch.ids[i])
			}
		}
	}
	if err := m.keepSplitRefs(ctx, repoPath, remoteID, out); err != nil {
		return nil, err
	}

	if walked {
		// Forget what is no longer reachable from the tips: everything
		// kept is reachable from the refs just written, so gc never
		// removes a commit the cache still names.
		for c := range cache.commits {
			if !reachable[c] {
				delete(cache.commits, c)
			}
		}
		keep := map[string]bool{}
		var stack []string
		for _, s := range cache.commits {
			if s != "" && !keep[s] {
				keep[s] = true
				stack = append(stack, s)
			}
		}
		for len(stack) > 0 {
			x := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if in := cache.info[x]; in != nil {
				for _, p := range in.parents {
					if !keep[p] {
						keep[p] = true
						stack = append(stack, p)
					}
				}
			}
		}
		for s := range cache.info {
			if !keep[s] {
				delete(cache.info, s)
			}
		}
		if err := cache.save(cachePath); err != nil {
			return nil, fmt.Errorf("save split cache: %w", err)
		}
	}
	return out, nil
}

// splitOne computes the split of hub commit c, whose parents have been
// split already. content is the new commit object, or nil when c maps to
// an existing split commit (or to none).
func splitOne(cf *gitexec.CatFile, cache *splitCache, format, c string, parents []string) (split string, content []byte, err error) {
	tree := ""
	if id, typ, _, ok, err := cf.Get(c + ":" + cache.prefix); err != nil {
		return "", nil, err
	} else if ok && typ == "tree" {
		tree = id
	}
	var nps []string
	for _, p := range parents {
		sp, known := cache.commits[p]
		if !known {
			return "", nil, errSplitCache
		}
		if sp == "" || contains(nps, sp) {
			continue
		}
		if cache.info[sp] == nil {
			return "", nil, errSplitCache
		}
		nps = append(nps, sp)
	}
	// Drop parents that are ancestors of another parent.
	if len(nps) > 1 {
		var reduced []string
		for i, a := range nps {
			redundant := false
			for j, b := range nps {
				if i != j && cache.isAncestor(a, b) {
					redundant = true
					break
				}
			}
			if !redundant {
				reduced = append(reduced, a)
			}
		}
		nps = reduced
	}
	switch {
	case tree == "" && len(nps) == 0:
		return "", nil, nil // the directory does not exist (yet)
	case tree == "":
		return nps[0], nil, nil // removed here: the mirror keeps its last state
	case len(nps) == 1 && cache.info[nps[0]].tree == tree:
		return nps[0], nil, nil // the directory did not change
	}
	_, typ, raw, ok, err := cf.Get(c)
	if err != nil {
		return "", nil, err
	}
	if !ok || typ != "commit" {
		return "", nil, fmt.Errorf("commit %s is missing", c)
	}
	headers, msg := splitObject(raw)
	var b bytes.Buffer
	b.WriteString("tree " + tree + "\n")
	for _, p := range nps {
		b.WriteString("parent " + p + "\n")
	}
	for _, h := range headers {
		// Signatures would not verify on the new commit; other headers
		// (mergetag, …) describe the hub commit.
		if strings.HasPrefix(h, "author ") || strings.HasPrefix(h, "committer ") || strings.HasPrefix(h, "encoding ") {
			b.WriteString(h + "\n")
		}
	}
	b.WriteString("\n")
	b.Write(msg)
	content = b.Bytes()
	split = gitexec.HashObject(format, "commit", content)
	gen := 1
	for _, p := range nps {
		if g := cache.info[p].gen + 1; g > gen {
			gen = g
		}
	}
	cache.info[split] = &splitInfo{tree: tree, gen: gen, parents: nps}
	return split, content, nil
}

// splitObject separates a commit or tag object's headers (continuation
// lines joined to their header) from its message.
func splitObject(raw []byte) (headers []string, msg []byte) {
	head := raw
	if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
		head, msg = raw[:i], raw[i+2:]
	}
	for _, line := range strings.Split(strings.TrimRight(string(head), "\n"), "\n") {
		if strings.HasPrefix(line, " ") && len(headers) > 0 {
			headers[len(headers)-1] += "\n" + line
			continue
		}
		headers = append(headers, line)
	}
	return headers, msg
}

// peelToCommit follows tag objects from val to a commit.
func peelToCommit(cf *gitexec.CatFile, val string) (peeled, bool, error) {
	var p peeled
	cur := val
	for i := 0; i < 10; i++ {
		_, typ, raw, ok, err := cf.Get(cur)
		if err != nil {
			return p, false, err
		}
		if !ok {
			return p, false, fmt.Errorf("object %s is missing", cur)
		}
		switch typ {
		case "commit":
			p.commit = cur
			return p, true, nil
		case "tag":
			if p.tag == nil {
				p.tag = raw
			}
			headers, _ := splitObject(raw)
			next := ""
			for _, h := range headers {
				if v, ok := strings.CutPrefix(h, "object "); ok {
					next = v
				}
			}
			if next == "" {
				return p, false, fmt.Errorf("tag %s names no object", cur)
			}
			cur = next
		default:
			return p, false, nil
		}
	}
	return p, false, fmt.Errorf("tag chain from %s is too long", val)
}

// rewriteTag makes an annotated tag pointing at commit, named name, with
// the original tagger and message (without its signature, which would not
// verify).
func rewriteTag(raw []byte, commit, name string) []byte {
	headers, msg := splitObject(raw)
	var b bytes.Buffer
	b.WriteString("object " + commit + "\ntype commit\ntag " + name + "\n")
	for _, h := range headers {
		if strings.HasPrefix(h, "tagger ") {
			b.WriteString(h + "\n")
		}
	}
	b.WriteString("\n")
	for _, marker := range []string{"-----BEGIN PGP SIGNATURE-----", "-----BEGIN SSH SIGNATURE-----", "-----BEGIN SIGNED MESSAGE-----"} {
		if i := bytes.Index(msg, []byte(marker)); i >= 0 && (i == 0 || msg[i-1] == '\n') {
			msg = msg[:i]
		}
	}
	b.Write(msg)
	return b.Bytes()
}

// keepSplitRefs points refs/gitrepo/split/<remote>/<mirror ref> at values
// and removes the remote's other split refs.
func (m *Manager) keepSplitRefs(ctx context.Context, repoPath string, remoteID int64, values map[string]string) error {
	prefix := splitRefPrefix(remoteID)
	cur, err := m.git.ForEachRef(ctx, repoPath, prefix)
	if err != nil {
		return err
	}
	var lines []string
	for ref, v := range values {
		if cur[prefix+ref] != v {
			lines = append(lines, "update "+prefix+ref+" "+v)
		}
	}
	for ref := range cur {
		if _, ok := values[strings.TrimPrefix(ref, prefix)]; !ok {
			lines = append(lines, "delete "+ref)
		}
	}
	sort.Strings(lines)
	return m.git.UpdateRefs(ctx, repoPath, lines)
}

// ForgetSplits removes a remote's split cache and refs (remote deleted, or
// its view changed), or every remote's when remoteID is 0 (backups purged:
// the splits may hold what was purged; they are rebuilt from the current
// history).
func (m *Manager) ForgetSplits(ctx context.Context, repoPath string, remoteID int64) error {
	refPrefix, cacheGlob := "refs/gitrepo/split/", filepath.Join(repoPath, "gitrepo", "split", "*.cache")
	if remoteID != 0 {
		refPrefix, cacheGlob = splitRefPrefix(remoteID), m.splitCachePath(repoPath, remoteID)
	}
	refs, err := m.git.ForEachRef(ctx, repoPath, refPrefix)
	if err != nil {
		return err
	}
	var lines []string
	for ref := range refs {
		lines = append(lines, "delete "+ref)
	}
	if err := m.git.UpdateRefs(ctx, repoPath, lines); err != nil {
		return err
	}
	files, _ := filepath.Glob(cacheGlob)
	for _, f := range files {
		os.Remove(f)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
