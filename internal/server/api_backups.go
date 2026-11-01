package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/qwq-inc/gitrepo/internal/syncer"
)

func (s *Server) registerBackupAPI(h func(string, http.HandlerFunc)) {
	h("GET /api/repos/{org}/{name}/backups", s.apiListBackups)
	h("POST /api/repos/{org}/{name}/backups/restore", s.apiRestoreBackup)
	h("POST /api/repos/{org}/{name}/backups/purge", s.apiPurgeBackups)
}

type backupOut struct {
	syncer.Backup
	Remote string `json:"remote,omitempty"`
}

func (s *Server) apiListBackups(w http.ResponseWriter, r *http.Request) {
	repo, err := s.repoFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	list, err := syncer.ListBackups(r.Context(), s.git, s.reg.Path(repo.Slug), s.cfg.Sync.BackupRetention.D())
	if err != nil {
		s.fail(w, err)
		return
	}
	labels := map[int64]string{}
	if remotes, err := s.store.ListRemotes(r.Context(), repo.ID); err == nil {
		for _, rem := range remotes {
			labels[rem.ID] = rem.Label()
		}
	}
	out := []backupOut{}
	for _, b := range list {
		o := backupOut{Backup: b}
		if b.Source == "remote" {
			o.Remote = labels[b.RemoteID]
		}
		out = append(out, o)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiRestoreBackup(w http.ResponseWriter, r *http.Request) {
	repo, err := s.repoFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	var in struct {
		Backup string `json:"backup"`
		Branch string `json:"branch"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	if in.Branch == "" {
		b, ok := parseBackupForName(in.Backup)
		if !ok {
			s.fail(w, badRequest("backup is required"))
			return
		}
		in.Branch = "restore/" + b
	}
	sha, err := s.mgr.RestoreBackup(r.Context(), repo, in.Backup, in.Branch)
	switch {
	case errors.Is(err, syncer.ErrNoSuchBackup):
		s.fail(w, &apiError{status: http.StatusNotFound, err: err})
		return
	case err != nil:
		s.fail(w, badRequest("%v", err))
		return
	}
	s.log.Info("backup restored", "repo", repo.Slug, "backup", in.Backup, "branch", in.Branch)
	noteAudit(r, "%s restored as %s at %s", in.Backup, in.Branch, short(sha))
	writeJSON(w, http.StatusOK, map[string]string{"branch": in.Branch, "sha": sha})
}

// parseBackupForName derives a default branch name such as
// "main-20261031T093000" from a backup ref.
func parseBackupForName(ref string) (string, bool) {
	rest, ok := strings.CutPrefix(ref, "refs/backup/")
	if !ok {
		return "", false
	}
	stamp, orig, ok := strings.Cut(rest, "/")
	if !ok {
		return "", false
	}
	if i := strings.LastIndex(orig, "refs/heads/"); i >= 0 {
		orig = orig[i+len("refs/heads/"):]
	} else if i := strings.LastIndex(orig, "refs/tags/"); i >= 0 {
		orig = "tag-" + orig[i+len("refs/tags/"):]
	}
	if t, err := time.Parse("20060102T150405.000Z", stamp); err == nil {
		stamp = t.UTC().Format("20060102T150405")
	}
	return orig + "-" + stamp, true
}

func (s *Server) apiPurgeBackups(w http.ResponseWriter, r *http.Request) {
	repo, err := s.repoFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	var in struct {
		Backups []string `json:"backups"`
		All     bool     `json:"all"`
		LFS     bool     `json:"lfs"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	if len(in.Backups) == 0 && !in.All {
		s.fail(w, badRequest("name the backups to purge, or set all=true"))
		return
	}
	if in.All {
		in.Backups = nil
	}
	res, err := s.mgr.PurgeBackups(r.Context(), repo, in.Backups, in.LFS)
	switch {
	case errors.Is(err, syncer.ErrNoSuchBackup):
		s.fail(w, &apiError{status: http.StatusNotFound, err: err})
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	s.log.Warn("backups purged", "repo", repo.Slug, "backups", res.Backups, "lfs_objects", res.LFSObjects)
	if err := s.store.SetRepoPurged(r.Context(), repo.ID); err != nil {
		s.log.Error("record purge", "repo", repo.Slug, "err", err)
	}
	noteAudit(r, "purged %d backup(s), %d LFS object(s)", res.Backups, res.LFSObjects)
	writeJSON(w, http.StatusOK, res)
}
