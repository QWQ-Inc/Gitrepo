package syncer

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/qwq-inc/gitrepo/internal/store"
)

// A View is what part of a hub repository one mirror receives. By default
// a mirror gets every branch and tag as they are. A view can instead:
//
//   - map refs: send only some branches or tags, possibly renamed
//     ("api:main" sends the hub's branch api as the mirror's main;
//     "refs/tags/api-v*:refs/tags/v*" renames tags). Commits are unchanged.
//   - split a subdirectory: the mirror gets the history of one directory of
//     the hub as its own repository, with that directory as its root (like
//     `git subtree split`). The split commits are new (and deterministic:
//     the same hub history always gives the same split), so changes made on
//     such a mirror cannot flow back; they show up as divergences.
//
// A monorepo can so be published as several repositories: by directory,
// or by branch.
type View struct {
	Maps []RefMap
	// Subdir is a path in the hub repository ("" = the whole tree).
	Subdir string
}

// RefMap sends hub refs matching Src to the mirror as Dst. Either both have
// one "*" (matching a non-empty part of the name) or neither has.
type RefMap struct {
	Src, Dst string
}

func (m RefMap) String() string { return m.Src + ":" + m.Dst }

// ViewOf returns a remote's view.
func ViewOf(rem *store.Remote) (View, error) { return ParseView(rem.RefMap, rem.Subdir) }

// ParseView checks a view's settings. A map entry is "SRC:DST" or "REF"
// (sent under the same name); a name without "refs/" is a branch.
func ParseView(maps []string, subdir string) (View, error) {
	var v View
	for _, s := range maps {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		src, dst, ok := strings.Cut(s, ":")
		if !ok {
			dst = src
		}
		m := RefMap{Src: fullRef(strings.TrimSpace(src)), Dst: fullRef(strings.TrimSpace(dst))}
		if err := m.check(); err != nil {
			return View{}, fmt.Errorf("%q: %w", s, err)
		}
		v.Maps = append(v.Maps, m)
	}
	sub, err := cleanSubdir(subdir)
	if err != nil {
		return View{}, err
	}
	v.Subdir = sub
	return v, nil
}

func fullRef(s string) string {
	if strings.HasPrefix(s, "refs/") {
		return s
	}
	return "refs/heads/" + s
}

func refKind(ref string) string {
	switch {
	case strings.HasPrefix(ref, "refs/heads/"):
		return "branch"
	case strings.HasPrefix(ref, "refs/tags/"):
		return "tag"
	}
	return ""
}

func (m RefMap) check() error {
	ks, kd := refKind(m.Src), refKind(m.Dst)
	if ks == "" || kd == "" {
		return errors.New("only branches and tags (refs/heads/…, refs/tags/…) can be mirrored")
	}
	if ks != kd {
		return errors.New("a branch must map to a branch and a tag to a tag")
	}
	if strings.Count(m.Src, "*") != strings.Count(m.Dst, "*") || strings.Count(m.Src, "*") > 1 {
		return errors.New(`use one "*" on both sides, or none`)
	}
	for _, r := range []string{m.Src, m.Dst} {
		name := strings.TrimPrefix(strings.TrimPrefix(r, "refs/heads/"), "refs/tags/")
		if name == "" || strings.ContainsAny(name, " \t\n~^:?[\\") || strings.Contains(name, "..") || strings.Contains(name, "//") ||
			strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".lock") || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "-") {
			return fmt.Errorf("invalid ref name %q", r)
		}
	}
	return nil
}

func cleanSubdir(s string) (string, error) {
	s = strings.Trim(strings.TrimSpace(s), "/")
	if s == "" {
		return "", nil
	}
	if strings.ContainsAny(s, "\n\\") {
		return "", fmt.Errorf("invalid subdirectory %q", s)
	}
	c := path.Clean(s)
	if c != s || c == "." || strings.HasPrefix(c, "../") || c == ".." {
		return "", fmt.Errorf("invalid subdirectory %q (use a relative path like services/api)", s)
	}
	for _, part := range strings.Split(c, "/") {
		if part == ".git" {
			return "", fmt.Errorf("invalid subdirectory %q", s)
		}
	}
	return c, nil
}

// IsZero reports whether the view is the default (everything, unchanged).
func (v View) IsZero() bool { return len(v.Maps) == 0 && v.Subdir == "" }

// effective returns the maps in force. Branches and tags are chosen
// separately: with no branch map the mirror gets every branch; with no tag
// map it gets every tag only when the view is otherwise the default (a
// split, or a mirror of chosen branches, gets no tags: the tags of a
// monorepo usually belong to one part of it, and would carry the history of
// the rest along).
func (v View) effective() []RefMap {
	var heads, tags bool
	for _, m := range v.Maps {
		switch refKind(m.Src) {
		case "branch":
			heads = true
		case "tag":
			tags = true
		}
	}
	maps := v.Maps[:len(v.Maps):len(v.Maps)]
	if !heads {
		maps = append(maps, RefMap{"refs/heads/*", "refs/heads/*"})
	}
	if !heads && !tags && v.Subdir == "" {
		maps = append(maps, RefMap{"refs/tags/*", "refs/tags/*"})
	}
	return maps
}

// Map returns the mirror's name for hub ref; ok is false when the view
// does not send it. The first matching map wins.
func (v View) Map(ref string) (string, bool) {
	for _, m := range v.effective() {
		if dst, ok := m.apply(ref); ok {
			return dst, true
		}
	}
	return "", false
}

func (m RefMap) apply(ref string) (string, bool) {
	i := strings.IndexByte(m.Src, '*')
	if i < 0 {
		return m.Dst, ref == m.Src
	}
	pre, suf := m.Src[:i], m.Src[i+1:]
	if len(ref) <= len(pre)+len(suf) || !strings.HasPrefix(ref, pre) || !strings.HasSuffix(ref, suf) {
		return "", false
	}
	mid := ref[len(pre) : len(ref)-len(suf)]
	return strings.Replace(m.Dst, "*", mid, 1), true
}

// Apply returns the refs the mirror should have: mirror ref → hub value,
// and mirror ref → hub ref. Two hub refs mapped to one mirror ref is an
// error (it would depend on chance which one wins).
func (v View) Apply(hub map[string]string) (want, from map[string]string, err error) {
	want, from = map[string]string{}, map[string]string{}
	refs := make([]string, 0, len(hub))
	for r := range hub {
		refs = append(refs, r)
	}
	sort.Strings(refs)
	for _, r := range refs {
		dst, ok := v.Map(r)
		if !ok {
			continue
		}
		if prev, dup := from[dst]; dup {
			return nil, nil, fmt.Errorf("both %s and %s map to %s on this mirror", prev, r, dst)
		}
		want[dst], from[dst] = hub[r], r
	}
	return want, from, nil
}

// DefaultBranch is the mirror's default branch: the hub's, under its
// mirror name, when the view sends it; otherwise the only branch the view
// sends; otherwise "".
func (v View) DefaultBranch(hubDefault string, want map[string]string) string {
	if hubDefault != "" {
		if dst, ok := v.Map("refs/heads/" + hubDefault); ok {
			if _, present := want[dst]; present {
				return strings.TrimPrefix(dst, "refs/heads/")
			}
		}
	}
	var only string
	for ref := range want {
		if strings.HasPrefix(ref, "refs/heads/") {
			if only != "" {
				return ""
			}
			only = ref
		}
	}
	return strings.TrimPrefix(only, "refs/heads/")
}

// Describe is a one-line summary for status output ("" for the default).
func (v View) Describe() string {
	if v.IsZero() {
		return ""
	}
	var parts, heads, tags []string
	if v.Subdir != "" {
		parts = append(parts, "directory "+v.Subdir+"/")
	}
	for _, m := range v.Maps {
		d := short(m.Src)
		if m.Dst != m.Src {
			d += " → " + short(m.Dst)
		}
		if refKind(m.Src) == "branch" {
			heads = append(heads, d)
		} else {
			tags = append(tags, d)
		}
	}
	if len(heads) == 0 {
		heads = []string{"every branch"}
	}
	if len(tags) == 0 {
		tags = []string{"no tags"}
	}
	return strings.Join(append(append(parts, heads...), tags...), ", ")
}

func short(ref string) string {
	if s, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return s
	}
	if s, ok := strings.CutPrefix(ref, "refs/tags/"); ok {
		return "tag " + s
	}
	return ref
}
