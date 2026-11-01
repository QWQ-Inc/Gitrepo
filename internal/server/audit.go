package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/qwq-inc/gitrepo/internal/auth"
	"github.com/qwq-inc/gitrepo/internal/gitexec"
	"github.com/qwq-inc/gitrepo/internal/gitserve"
	"github.com/qwq-inc/gitrepo/internal/store"
	"github.com/qwq-inc/gitrepo/internal/syncer"
)

// Every authenticated API request that may change something (any method
// but GET and HEAD) is written to the audit log with its route, path,
// status and the client address. Request bodies are never recorded: they
// carry credentials. Handlers add a short, secret-free description with
// noteAudit. Pushes are recorded by the post-receive handler.

type auditKey struct{}

type auditNote struct {
	repo   string
	detail string
}

// noteAudit describes the change a request made, for its audit entry.
func noteAudit(r *http.Request, format string, args ...any) {
	if n, ok := r.Context().Value(auditKey{}).(*auditNote); ok {
		n.detail = fmt.Sprintf(format, args...)
	}
}

// noteAuditRepo sets the repository of a request whose path does not name it.
func noteAuditRepo(r *http.Request, slug string) {
	if n, ok := r.Context().Value(auditKey{}).(*auditNote); ok {
		n.repo = slug
	}
}

func audited(r *http.Request) bool { return r.Method != http.MethodGet && r.Method != http.MethodHead }

// withAudit runs fn and records the request afterwards.
func (s *Server) withAudit(p *auth.Principal, fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		note := &auditNote{}
		if org, name := r.PathValue("org"), r.PathValue("name"); org != "" && name != "" {
			note.repo = org + "/" + name
		}
		sw := &statusWriter{ResponseWriter: w}
		start := time.Now()
		defer func() {
			status := sw.status
			if status == 0 {
				status = http.StatusOK
			}
			if rec := recover(); rec != nil {
				status = http.StatusInternalServerError
				defer panic(rec)
			}
			if msg := sw.errorMessage(); status >= 400 && msg != "" {
				if note.detail != "" {
					msg = note.detail + "; " + msg
				}
				note.detail = msg
			}
			s.recordAudit(&store.AuditEntry{
				At:         store.FromTime(start),
				Actor:      p.Name,
				Via:        "api",
				ClientAddr: gitserve.ClientAddr(r, s.cfg.TrustedProxyPrefixes()),
				Action:     r.Pattern,
				Path:       r.URL.Path,
				Repo:       note.repo,
				Status:     status,
				Detail:     note.detail,
			})
		}()
		fn(sw, r.WithContext(context.WithValue(r.Context(), auditKey{}, note)))
		if sw.status < 400 && s.clock != nil {
			s.clock.bump(note.repo) // edges pick up the change
		}
	}
}

// recordAudit stores an entry; the request it describes has already been
// served, so a failure is logged rather than reported.
func (s *Server) recordAudit(e *store.AuditEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.store.AddAudit(ctx, e); err != nil {
		s.log.Error("write audit log", "action", e.Action, "actor", e.Actor, "err", err)
	}
}

// auditPush records a push accepted by the hub. forced lists the refs whose
// previous value was overwritten or deleted (and backed up).
func (s *Server) auditPush(pusher, transport, client, slug string, updates []syncer.RefUpdate, forced []string) {
	isForced := map[string]bool{}
	for _, r := range forced {
		isForced[r] = true
	}
	var b strings.Builder
	for i, u := range updates {
		if i == 50 {
			fmt.Fprintf(&b, "; … %d more", len(updates)-i)
			break
		}
		if i > 0 {
			b.WriteString("; ")
		}
		ref := strings.TrimPrefix(u.Ref, "refs/heads/")
		switch {
		case gitexec.IsZero(u.Old):
			fmt.Fprintf(&b, "%s created at %s", ref, short(u.New))
		case gitexec.IsZero(u.New):
			fmt.Fprintf(&b, "%s deleted (was %s)", ref, short(u.Old))
		case isForced[u.Ref]:
			fmt.Fprintf(&b, "%s %s → %s (forced)", ref, short(u.Old), short(u.New))
		default:
			fmt.Fprintf(&b, "%s %s → %s", ref, short(u.Old), short(u.New))
		}
	}
	if transport == "" {
		transport = "http"
	}
	if pusher == "" {
		pusher = "unknown"
	}
	s.recordAudit(&store.AuditEntry{
		Actor: pusher, Via: transport, ClientAddr: client,
		Action: "git push", Repo: slug, Detail: b.String(),
	})
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// pruneAudit applies audit.retention.
func (s *Server) pruneAudit(ctx context.Context) {
	ret := s.cfg.Audit.Retention.D()
	if ret <= 0 {
		return
	}
	if n, err := s.store.PruneAudit(ctx, store.FromTime(time.Now().Add(-ret))); err != nil {
		s.log.Warn("prune audit log", "err", err)
	} else if n > 0 {
		s.log.Info("pruned audit log", "entries", n)
	}
}

// statusWriter remembers the response status and the start of an error
// response, whose message goes into the audit entry.
type statusWriter struct {
	http.ResponseWriter
	status  int
	errBody []byte
}

func (w *statusWriter) errorMessage() string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(w.errBody, &e) == nil && e.Error != "" {
		return "error: " + e.Error
	}
	return ""
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status >= 400 && len(w.errBody) < 2048 {
		w.errBody = append(w.errBody, b[:min(len(b), 2048-len(w.errBody))]...)
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("hijacking not supported")
}

// ------------------------------------------------------------------ API

type auditOut struct {
	*store.AuditEntry
	At time.Time `json:"at"`
}

func (s *Server) registerAuditAPI(h func(string, http.HandlerFunc)) {
	h("GET /api/audit", s.apiListAudit)
}

func (s *Server) apiListAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.AuditFilter{Repo: q.Get("repo"), Actor: q.Get("actor"), Action: q.Get("action")}
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			s.fail(w, badRequest("before must be an entry id"))
			return
		}
		f.Before = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 500 {
			s.fail(w, badRequest("limit must be between 1 and 500"))
			return
		}
		f.Limit = n
	}
	if f.Limit == 0 {
		f.Limit = 100
	}
	list, err := s.store.ListAudit(r.Context(), f)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := struct {
		Entries []auditOut `json:"entries"`
		// Next is the "before" value for the following page, 0 at the end.
		Next int64 `json:"next,omitempty"`
	}{Entries: make([]auditOut, 0, len(list))}
	for _, e := range list {
		out.Entries = append(out.Entries, auditOut{AuditEntry: e, At: store.ToTime(e.At).UTC()})
	}
	if len(list) == f.Limit {
		out.Next = list[len(list)-1].ID
	}
	writeJSON(w, http.StatusOK, out)
}
