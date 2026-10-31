package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/qwq-inc/gitrepo/internal/githooks"
	"github.com/qwq-inc/gitrepo/internal/store"
	"github.com/qwq-inc/gitrepo/internal/syncer"
)

// internalHandler serves the unix socket used by hooks and askpass. The
// socket is mode 0600; requests additionally carry a per-process hook token
// or a one-time askpass ticket.
func (s *Server) internalHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/askpass", s.handleAskpass)
	mux.HandleFunc("POST /internal/post-receive", s.handlePostReceive)
	return mux
}

func (s *Server) handleAskpass(w http.ResponseWriter, r *http.Request) {
	var req githooks.AskpassRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	answer, ok := s.tickets.Answer(req.Ticket, req.Prompt)
	if !ok {
		http.Error(w, "unknown ticket", http.StatusForbidden)
		return
	}
	io.WriteString(w, answer)
}

func (s *Server) handlePostReceive(w http.ResponseWriter, r *http.Request) {
	var req githooks.PostReceiveRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.hookToken)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	repo, err := s.store.GetRepoBySlug(ctx, req.Repo)
	if err != nil {
		http.Error(w, "unknown repository", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	flusher, _ := w.(http.Flusher)
	say := func(format string, args ...any) {
		fmt.Fprintf(w, "repohub: "+format+"\n", args...)
		if flusher != nil {
			flusher.Flush()
		}
	}

	path := s.reg.Path(repo.Slug)
	updates := make([]syncer.RefUpdate, 0, len(req.Updates))
	for _, u := range req.Updates {
		updates = append(updates, syncer.RefUpdate{Old: u.Old, New: u.New, Ref: u.Ref})
	}
	if n, err := syncer.BackupHubUpdates(ctx, s.git, path, updates); err != nil {
		s.log.Error("back up overwritten refs", "repo", repo.Slug, "err", err)
		say("warning: could not back up overwritten refs: %v", err)
	} else if n > 0 {
		say("kept %d overwritten/deleted ref(s) under refs/backup/ on the hub", n)
	}
	if err := s.fixHead(ctx, repo, path, updates); err != nil {
		s.log.Warn("update HEAD", "repo", repo.Slug, "err", err)
	}

	remotes, err := s.store.ListRemotes(ctx, repo.ID)
	if err != nil {
		say("warning: %v", err)
		return
	}
	if len(remotes) == 0 {
		say("no mirrors configured for %s; the push is stored on the hub only", repo.Slug)
		say("add one with `rh remote add %s <target>` or `rh bind %s <target>`", repo.Slug, strings.SplitN(repo.Slug, "/", 2)[0])
		return
	}

	events := s.mgr.Enqueue(repo.ID, syncer.Request{Trigger: syncer.TriggerPush})
	if req.WaitMS <= 0 {
		say("sync to %d mirror(s) queued", len(remotes))
		return
	}
	say("syncing to %d mirror(s)…", len(remotes))
	width := 0
	for _, rem := range remotes {
		if l := len(rem.Label()); l > width {
			width = l
		}
	}
	timer := time.NewTimer(time.Duration(req.WaitMS) * time.Millisecond)
	defer timer.Stop()
	counts := map[string]int{}
	for {
		select {
		case e, ok := <-events:
			if !ok {
				return
			}
			switch e.Type {
			case "remote":
				counts[e.State]++
				writeRemoteEvent(say, e, width)
			case "done":
				if e.Error != "" {
					say("sync did not run: %s", e.Error)
					return
				}
				if counts[store.StateDiverged] > 0 {
					say("diverged mirrors were left untouched. Pull the mirror's commits into your branch and push again,")
					say("or overwrite them (the old state is kept under refs/backup/): rh sync %s --overwrite", repo.Slug)
				}
				if counts[store.StateFailed] > 0 {
					say("failed mirrors are retried automatically; see `rh status %s`", repo.Slug)
				}
				return
			}
		case <-timer.C:
			say("still syncing in the background; follow up with `rh status %s`", repo.Slug)
			return
		case <-ctx.Done():
			return
		}
	}
}

func writeRemoteEvent(say func(string, ...any), e syncer.Event, width int) {
	label := fmt.Sprintf("%-*s", width, e.Remote)
	switch e.State {
	case store.StateSynced:
		if e.Changed == 0 {
			say("  [ok]       %s  already up to date", label)
		} else {
			say("  [ok]       %s  %d ref(s) updated", label, e.Changed)
		}
	case store.StateDiverged:
		say("  [diverged] %s  %s", label, strings.Join(e.Divergent, ", "))
	case store.StateFailed:
		say("  [failed]   %s  %s", label, firstLine(e.Error))
	case store.StatePaused:
		say("  [paused]   %s", label)
	default:
		say("  [%s] %s", e.State, label)
	}
	for _, w := range e.Warnings {
		say("             %s  warning: %s", strings.Repeat(" ", width), firstLine(w))
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// fixHead makes sure HEAD points at an existing branch after a push (a fresh
// bare repository's HEAD names a branch that may never be pushed) and keeps
// repos.default_branch in sync with it.
func (s *Server) fixHead(ctx context.Context, repo *store.Repo, path string, updates []syncer.RefUpdate) error {
	head, err := s.git.SymbolicRef(ctx, path)
	if err != nil {
		return err
	}
	refs, err := s.git.ForEachRef(ctx, path, "refs/heads/")
	if err != nil {
		return err
	}
	if _, ok := refs[head]; !ok && len(refs) > 0 {
		head = ""
		for _, c := range []string{"refs/heads/main", "refs/heads/master"} {
			if _, ok := refs[c]; ok {
				head = c
				break
			}
		}
		if head == "" {
			for _, u := range updates {
				if _, ok := refs[u.Ref]; ok && strings.HasPrefix(u.Ref, "refs/heads/") {
					head = u.Ref
					break
				}
			}
		}
		if head == "" {
			names := make([]string, 0, len(refs))
			for r := range refs {
				names = append(names, r)
			}
			sort.Strings(names)
			head = names[0]
		}
		if err := s.git.SetHead(ctx, path, head); err != nil {
			return err
		}
	}
	branch := strings.TrimPrefix(head, "refs/heads/")
	if _, ok := refs[head]; ok && repo.DefaultBranch != branch {
		repo.DefaultBranch = branch
		return s.store.UpdateRepo(ctx, repo)
	}
	return nil
}
