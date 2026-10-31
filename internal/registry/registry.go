// Package registry creates and removes hub repositories and their remotes.
package registry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/qwq-inc/gitrepo/internal/gitexec"
	"github.com/qwq-inc/gitrepo/internal/provider"
	"github.com/qwq-inc/gitrepo/internal/store"
)

type Registry struct {
	Store    *store.Store
	Git      *gitexec.Git
	ReposDir string
	TrashDir string

	mu sync.Mutex
}

var segment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// Org names that would collide with server routes.
var reserved = map[string]bool{"api": true, "healthz": true, "metrics": true, "hooks": true, "archive": true, "internal": true, "static": true, "ui": true}

// ParseSlug validates "org/name" (an optional ".git" suffix is stripped).
func ParseSlug(slug string) (org, name string, err error) {
	slug = strings.TrimSuffix(strings.Trim(slug, "/"), ".git")
	org, name, ok := strings.Cut(slug, "/")
	if !ok || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("repository must be <org>/<name>, got %q", slug)
	}
	for _, s := range []string{org, name} {
		if !segment.MatchString(s) || strings.Contains(s, "..") || strings.HasSuffix(s, ".git") || strings.HasSuffix(s, ".lock") {
			return "", "", fmt.Errorf("invalid repository name segment %q (allowed: letters, digits, '.', '_', '-')", s)
		}
	}
	if reserved[strings.ToLower(org)] {
		return "", "", fmt.Errorf("%q is a reserved name", org)
	}
	return org, name, nil
}

// Path returns the bare repository directory for slug.
func (r *Registry) Path(slug string) string {
	return filepath.Join(r.ReposDir, filepath.FromSlash(slug)+".git")
}

// CreateRepo initialises the bare repository and registers it. When
// withBindings is set, remotes are created from the bindings matching the
// org. Creating an already-registered repository returns it unchanged.
func (r *Registry) CreateRepo(ctx context.Context, slug, visibility string, withBindings bool) (*store.Repo, bool, error) {
	org, name, err := ParseSlug(slug)
	if err != nil {
		return nil, false, err
	}
	slug = org + "/" + name
	if visibility == "" {
		visibility = "private"
	}
	if visibility != "private" && visibility != "public" && visibility != "internal" {
		return nil, false, fmt.Errorf("visibility must be private, public or internal")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, err := r.Store.GetRepoBySlug(ctx, slug); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, false, err
	}
	path := r.Path(slug)
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err != nil {
		if err := r.Git.InitBare(ctx, path, "main"); err != nil {
			return nil, false, fmt.Errorf("init bare repository: %w", err)
		}
	}
	repo := &store.Repo{Slug: slug, Visibility: visibility}
	if err := r.Store.CreateRepo(ctx, repo); err != nil {
		return nil, false, err
	}
	if withBindings {
		bindings, err := r.Store.ListBindings(ctx, org)
		if err != nil {
			return nil, false, err
		}
		for _, b := range bindings {
			if _, err := r.addBindingRemote(ctx, repo, b); err != nil && !errors.Is(err, ErrLocationTaken) {
				return nil, false, fmt.Errorf("create remote for binding %s: %w", b.TargetName, err)
			}
		}
	}
	return repo, true, nil
}

// ErrLocationTaken means another hub repository already mirrors to the
// requested place; two hub repositories pushing to one mirror would fight
// over it forever.
var ErrLocationTaken = errors.New("mirror location already in use")

// LocationConflict reports which repository already uses a mirror location.
func (r *Registry) LocationConflict(ctx context.Context, repoID, targetID int64, namespace, name, url string) error {
	other, err := r.Store.RemoteUsingLocation(ctx, repoID, targetID, namespace, name, url)
	if err != nil {
		return err
	}
	if other == nil {
		return nil
	}
	owner, err := r.Store.GetRepo(ctx, other.RepoID)
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: %s is already the mirror of %s", ErrLocationTaken, other.Label(), owner.Slug)
}

// addBindingRemote creates the remote a binding implies for repo. Remotes
// created this way never adopt a non-empty pre-existing repository on their
// own: an administrator has to confirm that (see Remote.Adopt).
func (r *Registry) addBindingRemote(ctx context.Context, repo *store.Repo, b *store.Binding) (*store.Remote, error) {
	org, name, _ := strings.Cut(repo.Slug, "/")
	rem := &store.Remote{
		RepoID:       repo.ID,
		TargetID:     b.TargetID,
		Namespace:    b.Namespace,
		Name:         b.MirrorName(org, name),
		OnDivergence: b.OnDivergence,
		Enabled:      true,
	}
	if err := r.LocationConflict(ctx, repo.ID, rem.TargetID, rem.Namespace, rem.Name, ""); err != nil {
		return nil, err
	}
	if err := r.Store.CreateRemote(ctx, rem); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, ErrLocationTaken
		}
		return nil, err
	}
	return rem, nil
}

// ApplyBinding adds the remote implied by b to an existing repository.
func (r *Registry) ApplyBinding(ctx context.Context, repo *store.Repo, b *store.Binding) (*store.Remote, error) {
	return r.addBindingRemote(ctx, repo, b)
}

// AddRemote attaches a target to a repository. adopt allows the first sync
// to write into a mirror repository that already contains other refs.
func (r *Registry) AddRemote(ctx context.Context, repo *store.Repo, targetName, namespace, name, url, onDivergence string, adopt bool) (*store.Remote, error) {
	t, err := r.Store.GetTargetByName(ctx, targetName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("target %q not found", targetName)
		}
		return nil, err
	}
	if name == "" {
		_, name, _ = strings.Cut(repo.Slug, "/")
	}
	if onDivergence == "" {
		onDivergence = store.OnDivergenceAlert
	}
	if onDivergence != store.OnDivergenceAlert && onDivergence != store.OnDivergenceOverwrite {
		return nil, fmt.Errorf("on_divergence must be alert or overwrite")
	}
	p, err := provider.Get(t.Provider)
	if err != nil {
		return nil, err
	}
	if url == "" {
		if _, err := p.RepoURL(t, namespace, name); err != nil {
			return nil, err
		}
	}
	if err := r.LocationConflict(ctx, repo.ID, t.ID, namespace, name, url); err != nil {
		return nil, err
	}
	rem := &store.Remote{
		RepoID: repo.ID, TargetID: t.ID, Namespace: namespace, Name: name, URL: url,
		OnDivergence: onDivergence, Enabled: true, Adopt: adopt,
	}
	if err := r.Store.CreateRemote(ctx, rem); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, fmt.Errorf("remote already exists")
		}
		return nil, err
	}
	return r.Store.GetRemote(ctx, rem.ID)
}

// DeleteRepo unregisters a repository and moves its data to the trash
// directory (nothing is destroyed).
func (r *Registry) DeleteRepo(ctx context.Context, repo *store.Repo) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.Store.DeleteRepo(ctx, repo.ID); err != nil {
		return "", err
	}
	src := r.Path(repo.Slug)
	dst := filepath.Join(r.TrashDir, strings.ReplaceAll(repo.Slug, "/", "__")+"-"+time.Now().UTC().Format("20060102T150405Z")+".git")
	if err := os.MkdirAll(r.TrashDir, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(src, dst); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return dst, nil
}
