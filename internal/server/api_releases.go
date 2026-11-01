package server

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/qwq-inc/gitrepo/internal/auth"
	"github.com/qwq-inc/gitrepo/internal/gitserve"
	"github.com/qwq-inc/gitrepo/internal/releases"
	"github.com/qwq-inc/gitrepo/internal/store"
)

// Release endpoints are authorized per repository like git itself, not
// with the admin scope: a CI token with write access to a repository can
// publish its releases; anyone who can clone can read them.

type repoHandler func(w http.ResponseWriter, r *http.Request, repo *store.Repo)

func (s *Server) registerReleaseAPI(mux *http.ServeMux) {
	read := func(pattern string, fn repoHandler) { mux.HandleFunc(pattern, s.repoAccess(false, fn)) }
	write := func(pattern string, fn repoHandler) { mux.HandleFunc(pattern, s.repoAccess(true, fn)) }
	read("GET /api/repos/{org}/{name}/releases", s.apiListReleases)
	write("POST /api/repos/{org}/{name}/releases", s.apiCreateRelease)
	read("GET /api/repos/{org}/{name}/releases/{tag}", s.apiGetRelease)
	write("PATCH /api/repos/{org}/{name}/releases/{tag}", s.apiUpdateRelease)
	write("DELETE /api/repos/{org}/{name}/releases/{tag}", s.apiDeleteRelease)
	write("POST /api/repos/{org}/{name}/releases/{tag}/publish", s.apiPublishRelease)
	write("PUT /api/repos/{org}/{name}/releases/{tag}/assets/{file}", s.apiPutAsset)
	read("GET /api/repos/{org}/{name}/releases/{tag}/assets/{file}", s.apiGetAsset)
	write("DELETE /api/repos/{org}/{name}/releases/{tag}/assets/{file}", s.apiDeleteAsset)
}

// repoAccess authorizes a request for the repository in its path with the
// git access rules (write needs the write scope on it) and audits changes.
func (s *Server) repoAccess(write bool, fn repoHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.auth.FromRequest(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="gitrepo"`)
			writeErr(w, http.StatusUnauthorized, errors.New("invalid token"))
			return
		}
		next := func(w http.ResponseWriter, r *http.Request) {
			slug := r.PathValue("org") + "/" + r.PathValue("name")
			repo, err := s.gitsrv.Access(r.Context(), slug, p, write, false)
			switch {
			case errors.Is(err, gitserve.ErrUnauthenticated):
				w.Header().Set("WWW-Authenticate", `Bearer realm="gitrepo"`)
				writeErr(w, http.StatusUnauthorized, errors.New("authentication required"))
			case errors.Is(err, gitserve.ErrNotFound):
				writeErr(w, http.StatusNotFound, errors.New("repository not found"))
			case errors.Is(err, gitserve.ErrForbidden):
				writeErr(w, http.StatusForbidden, err)
			case err != nil:
				s.fail(w, err)
			default:
				acc := &releaseAccess{p: p, write: p != nil && p.Can(auth.ScopeWrite, repo.Slug)}
				fn(w, r.WithContext(context.WithValue(r.Context(), releaseAccessKey{}, acc)), repo)
			}
		}
		if p != nil && audited(r) {
			next = s.withAudit(p, next)
		}
		next(w, r)
	}
}

type releaseAccessKey struct{}

// releaseAccess is who is asking: readers without write access see only
// published releases and no mirror details (those can name internal hosts
// and carry platform error messages).
type releaseAccess struct {
	p     *auth.Principal
	write bool
}

func accessOf(r *http.Request) *releaseAccess {
	if a, ok := r.Context().Value(releaseAccessKey{}).(*releaseAccess); ok {
		return a
	}
	return &releaseAccess{}
}

// requireAdminForAdopt: taking over a release someone made by hand changes
// content Gitrepo does not own, with the target's (admin-configured)
// credentials, so a repository write token is not enough.
func requireAdminForAdopt(r *http.Request, adopt *bool) error {
	if adopt != nil && *adopt {
		if a := accessOf(r); a.p == nil || !a.p.IsAdmin() {
			return &apiError{status: http.StatusForbidden, err: errors.New("adopting releases made by others needs an admin token")}
		}
	}
	return nil
}

type releaseAssetOut struct {
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	ContentType string    `json:"content_type"`
	URL         string    `json:"url"`
	CreatedAt   time.Time `json:"created_at"`
}

type releaseMirrorOut struct {
	RemoteID    int64      `json:"remote_id"`
	Mirror      string     `json:"mirror"`
	State       string     `json:"state"`
	Error       string     `json:"error,omitempty"`
	UpdatedAt   *time.Time `json:"updated_at,omitempty"`
	NextRetryAt *time.Time `json:"next_retry_at,omitempty"`
}

type releaseOut struct {
	Tag        string             `json:"tag"`
	Name       string             `json:"name"`
	Body       string             `json:"body"`
	Prerelease bool               `json:"prerelease"`
	Published  bool               `json:"published"`
	Adopt      bool               `json:"adopt,omitempty"`
	Assets     []releaseAssetOut  `json:"assets"`
	Mirrors    []releaseMirrorOut `json:"mirrors"`
	CreatedAt  time.Time          `json:"created_at"`
	UpdatedAt  time.Time          `json:"updated_at"`
	// Results of a synchronous publish or delete.
	Results []releases.Result `json:"results,omitempty"`
}

func (s *Server) releaseOut(ctx context.Context, repo *store.Repo, rel *store.Release, withMirrors bool) (*releaseOut, error) {
	out := &releaseOut{
		Tag: rel.Tag, Name: rel.Name, Body: rel.Body, Prerelease: rel.Prerelease, Published: rel.Published, Adopt: rel.Adopt,
		CreatedAt: store.ToTime(rel.CreatedAt).UTC(), UpdatedAt: store.ToTime(rel.UpdatedAt).UTC(),
		Assets: []releaseAssetOut{}, Mirrors: []releaseMirrorOut{},
	}
	assets, err := s.store.ListReleaseAssets(ctx, rel.ID)
	if err != nil {
		return nil, err
	}
	base := "/api/repos/" + repo.Slug + "/releases/" + escapePathSegment(rel.Tag) + "/assets/"
	for _, a := range assets {
		out.Assets = append(out.Assets, releaseAssetOut{
			Name: a.Name, Size: a.Size, SHA256: a.SHA256, ContentType: a.ContentType,
			URL: s.cfg.PublicURL + base + escapePathSegment(a.Name), CreatedAt: store.ToTime(a.CreatedAt).UTC(),
		})
	}
	if !withMirrors {
		return out, nil
	}
	remotes, err := s.store.ListRemotes(ctx, repo.ID)
	if err != nil {
		return nil, err
	}
	states, err := s.store.ListReleaseMirrors(ctx, rel.ID)
	if err != nil {
		return nil, err
	}
	byRemote := map[int64]*store.ReleaseMirror{}
	for _, st := range states {
		byRemote[st.RemoteID] = st
	}
	for _, rem := range remotes {
		mo := releaseMirrorOut{RemoteID: rem.ID, Mirror: rem.Label(), State: store.RelPending}
		if st := byRemote[rem.ID]; st != nil {
			mo.State, mo.Error, mo.UpdatedAt, mo.NextRetryAt = st.State, st.LastError, ts(st.UpdatedAt), ts(st.NextRetryAt)
		}
		if !rel.Published && (mo.State == store.RelPending || byRemote[rem.ID] == nil) {
			mo.State = "draft"
		}
		if !rem.Enabled {
			mo.State = "paused"
		}
		out.Mirrors = append(out.Mirrors, mo)
	}
	return out, nil
}

func escapePathSegment(s string) string { return url.PathEscape(s) }

// releaseFromPath loads the release named in the path. Drafts exist only
// for callers with write access.
func (s *Server) releaseFromPath(r *http.Request, repo *store.Repo) (*store.Release, error) {
	rel, err := s.store.GetReleaseByTag(r.Context(), repo.ID, r.PathValue("tag"))
	if errors.Is(err, store.ErrNotFound) || err == nil && !rel.Published && !accessOf(r).write {
		return nil, &apiError{status: http.StatusNotFound, err: fmt.Errorf("no release for tag %q", r.PathValue("tag"))}
	}
	return rel, err
}

func (s *Server) apiListReleases(w http.ResponseWriter, r *http.Request, repo *store.Repo) {
	rels, err := s.store.ListReleases(r.Context(), repo.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []*releaseOut{}
	writer := accessOf(r).write
	for _, rel := range rels {
		if !rel.Published && !writer {
			continue
		}
		ro, err := s.releaseOut(r.Context(), repo, rel, writer)
		if err != nil {
			s.fail(w, err)
			return
		}
		out = append(out, ro)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiGetRelease(w http.ResponseWriter, r *http.Request, repo *store.Repo) {
	rel, err := s.releaseFromPath(r, repo)
	if err != nil {
		s.fail(w, err)
		return
	}
	ro, err := s.releaseOut(r.Context(), repo, rel, accessOf(r).write)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ro)
}

const maxReleaseBody = 100_000

type releaseIn struct {
	Tag        *string `json:"tag"`
	Name       *string `json:"name"`
	Body       *string `json:"body"`
	Prerelease *bool   `json:"prerelease"`
	Publish    *bool   `json:"publish"`
	Adopt      *bool   `json:"adopt"`
	// Wait publishes synchronously and returns per-mirror results.
	Wait bool `json:"wait"`
}

func (in *releaseIn) apply(rel *store.Release) error {
	if in.Name != nil {
		if utf8.RuneCountInString(*in.Name) > 255 {
			return badRequest("name is longer than 255 characters")
		}
		rel.Name = *in.Name
	}
	if in.Body != nil {
		if len(*in.Body) > maxReleaseBody {
			return badRequest("notes are longer than %d bytes", maxReleaseBody)
		}
		rel.Body = *in.Body
	}
	if in.Prerelease != nil {
		rel.Prerelease = *in.Prerelease
	}
	if in.Adopt != nil {
		rel.Adopt = *in.Adopt
	}
	if in.Publish != nil {
		if rel.Published && !*in.Publish {
			return badRequest("a published release cannot be unpublished; delete it instead")
		}
		rel.Published = *in.Publish
	}
	return nil
}

func (s *Server) apiCreateRelease(w http.ResponseWriter, r *http.Request, repo *store.Repo) {
	var in releaseIn
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	if in.Tag == nil || *in.Tag == "" {
		s.fail(w, badRequest("tag is required"))
		return
	}
	if err := requireAdminForAdopt(r, in.Adopt); err != nil {
		s.fail(w, err)
		return
	}
	tag := *in.Tag
	refs, err := s.git.ForEachRef(r.Context(), s.reg.Path(repo.Slug), "refs/tags/"+tag)
	if err != nil {
		s.fail(w, err)
		return
	}
	if _, ok := refs["refs/tags/"+tag]; !ok {
		s.fail(w, badRequest("tag %q does not exist on the hub; push it first (git push hub %s)", tag, tag))
		return
	}
	rel := &store.Release{RepoID: repo.ID, Tag: tag, Name: tag}
	if err := in.apply(rel); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.CreateRelease(r.Context(), rel); err != nil {
		if errors.Is(err, store.ErrConflict) {
			err = &apiError{status: http.StatusConflict, err: fmt.Errorf("a release for %s already exists", tag)}
		}
		s.fail(w, err)
		return
	}
	noteAudit(r, "release %s (published %t)", rel.Tag, rel.Published)
	s.respondRelease(w, r, repo, rel, http.StatusCreated, in.Wait, false)
}

func (s *Server) apiUpdateRelease(w http.ResponseWriter, r *http.Request, repo *store.Repo) {
	rel, err := s.releaseFromPath(r, repo)
	if err != nil {
		s.fail(w, err)
		return
	}
	var in releaseIn
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	if in.Tag != nil && *in.Tag != rel.Tag {
		s.fail(w, badRequest("the tag of a release cannot be changed"))
		return
	}
	if err := requireAdminForAdopt(r, in.Adopt); err != nil {
		s.fail(w, err)
		return
	}
	if err := in.apply(rel); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.UpdateRelease(r.Context(), rel); err != nil {
		s.fail(w, err)
		return
	}
	noteAudit(r, "release %s (published %t)", rel.Tag, rel.Published)
	s.respondRelease(w, r, repo, rel, http.StatusOK, in.Wait, false)
}

func (s *Server) apiPublishRelease(w http.ResponseWriter, r *http.Request, repo *store.Repo) {
	rel, err := s.releaseFromPath(r, repo)
	if err != nil {
		s.fail(w, err)
		return
	}
	var in struct {
		Wait  bool  `json:"wait"`
		Force bool  `json:"force"`
		Adopt *bool `json:"adopt"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	if err := requireAdminForAdopt(r, in.Adopt); err != nil {
		s.fail(w, err)
		return
	}
	rel.Published = true
	if in.Adopt != nil {
		rel.Adopt = *in.Adopt
	}
	if err := s.store.UpdateRelease(r.Context(), rel); err != nil {
		s.fail(w, err)
		return
	}
	noteAudit(r, "release %s (adopt %t, force %t)", rel.Tag, rel.Adopt, in.Force)
	s.respondRelease(w, r, repo, rel, http.StatusOK, in.Wait, in.Force)
}

// respondRelease publishes (synchronously with wait, otherwise in the
// background) and answers with the release.
func (s *Server) respondRelease(w http.ResponseWriter, r *http.Request, repo *store.Repo, rel *store.Release, status int, wait, force bool) {
	var results []releases.Result
	if rel.Published {
		if wait {
			var err error
			if results, err = s.releases.Publish(r.Context(), rel.ID, releases.PublishOptions{Force: force, Now: true}); err != nil {
				s.fail(w, err)
				return
			}
		} else {
			s.releases.Enqueue(rel.ID)
		}
	}
	ro, err := s.releaseOut(r.Context(), repo, rel, true)
	if err != nil {
		s.fail(w, err)
		return
	}
	ro.Results = results
	writeJSON(w, status, ro)
}

func (s *Server) apiDeleteRelease(w http.ResponseWriter, r *http.Request, repo *store.Repo) {
	rel, err := s.releaseFromPath(r, repo)
	if err != nil {
		s.fail(w, err)
		return
	}
	hubOnly, _ := strconv.ParseBool(r.URL.Query().Get("hub_only"))
	noteAudit(r, "release %s (hub only %t)", rel.Tag, hubOnly)
	results, err := s.releases.Delete(r.Context(), repo, rel, hubOnly)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "results": results})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": rel.Tag, "results": results})
}

func (s *Server) apiPutAsset(w http.ResponseWriter, r *http.Request, repo *store.Repo) {
	rel, err := s.releaseFromPath(r, repo)
	if err != nil {
		s.fail(w, err)
		return
	}
	name := r.PathValue("file")
	ct := r.Header.Get("Content-Type")
	if ct == "" || ct == "application/x-www-form-urlencoded" {
		ct = mime.TypeByExtension(filepath.Ext(name))
	}
	if limit := int64(s.cfg.Releases.MaxAssetSize); limit > 0 && r.ContentLength > limit {
		s.fail(w, &apiError{status: http.StatusRequestEntityTooLarge, err: fmt.Errorf("%s is larger than releases.max_asset_size (%d bytes)", name, limit)})
		return
	}
	a, err := s.releases.PutAsset(r.Context(), repo, rel, name, ct, r.Body)
	switch {
	case errors.Is(err, releases.ErrInvalidName):
		s.fail(w, badRequest("invalid file name %q", name))
		return
	case errors.Is(err, releases.ErrTooLarge):
		s.fail(w, &apiError{status: http.StatusRequestEntityTooLarge, err: fmt.Errorf("%s is larger than releases.max_asset_size", name)})
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	noteAudit(r, "release %s: %s (%d bytes, sha256 %s)", rel.Tag, a.Name, a.Size, a.SHA256[:12])
	writeJSON(w, http.StatusCreated, releaseAssetOut{
		Name: a.Name, Size: a.Size, SHA256: a.SHA256, ContentType: a.ContentType, CreatedAt: store.ToTime(a.CreatedAt).UTC(),
		URL: s.cfg.PublicURL + "/api/repos/" + repo.Slug + "/releases/" + escapePathSegment(rel.Tag) + "/assets/" + escapePathSegment(a.Name),
	})
}

func (s *Server) apiGetAsset(w http.ResponseWriter, r *http.Request, repo *store.Repo) {
	rel, err := s.releaseFromPath(r, repo)
	if err != nil {
		s.fail(w, err)
		return
	}
	assets, err := s.store.ListReleaseAssets(r.Context(), rel.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, a := range assets {
		if a.Name != r.PathValue("file") {
			continue
		}
		f, err := s.releases.OpenAsset(repo, a)
		if err != nil {
			s.fail(w, err)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", a.ContentType)
		w.Header().Set("ETag", `"`+a.SHA256+`"`)
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": a.Name}))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if repo.Visibility == "public" {
			w.Header().Set("Cache-Control", "public, max-age=300")
		} else {
			w.Header().Set("Cache-Control", "private, max-age=300")
		}
		http.ServeContent(w, r, "", store.ToTime(a.CreatedAt), f)
		return
	}
	s.fail(w, &apiError{status: http.StatusNotFound, err: fmt.Errorf("release %s has no file %q", rel.Tag, r.PathValue("file"))})
}

func (s *Server) apiDeleteAsset(w http.ResponseWriter, r *http.Request, repo *store.Repo) {
	rel, err := s.releaseFromPath(r, repo)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.releases.RemoveAsset(r.Context(), repo, rel, r.PathValue("file")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			err = &apiError{status: http.StatusNotFound, err: fmt.Errorf("release %s has no file %q", rel.Tag, r.PathValue("file"))}
		}
		s.fail(w, err)
		return
	}
	noteAudit(r, "release %s: %s", rel.Tag, r.PathValue("file"))
	w.WriteHeader(http.StatusNoContent)
}
