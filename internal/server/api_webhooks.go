package server

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/qwq-inc/gitrepo/internal/auth"
	"github.com/qwq-inc/gitrepo/internal/notify"
	"github.com/qwq-inc/gitrepo/internal/store"
)

func (s *Server) registerWebhookAPI(h func(string, http.HandlerFunc)) {
	h("GET /api/webhooks", s.apiListWebhooks)
	h("POST /api/webhooks", s.apiCreateWebhook)
	h("PATCH /api/webhooks/{id}", s.apiUpdateWebhook)
	h("DELETE /api/webhooks/{id}", s.apiDeleteWebhook)
	h("POST /api/webhooks/{id}/test", s.apiTestWebhook)
}

type webhookOut struct {
	ID             int64      `json:"id"`
	Name           string     `json:"name"`
	Host           string     `json:"host"`
	Format         string     `json:"format"`
	Events         []string   `json:"events"`
	Repos          []string   `json:"repos,omitempty"`
	Signed         bool       `json:"signed"`
	Enabled        bool       `json:"enabled"`
	LastStatus     string     `json:"last_status,omitempty"`
	LastDeliveryAt *time.Time `json:"last_delivery_at,omitempty"`
}

func toWebhookOut(w *store.Webhook) webhookOut {
	events := w.Events
	if len(events) == 0 {
		events = []string{"*"}
	}
	return webhookOut{ID: w.ID, Name: w.Name, Host: w.URLHost, Format: w.Format, Events: events, Repos: w.Repos,
		Signed: len(w.Secret) > 0, Enabled: w.Enabled, LastStatus: w.LastStatus, LastDeliveryAt: ts(w.LastDeliveryAt)}
}

func (s *Server) apiListWebhooks(w http.ResponseWriter, r *http.Request) {
	hooks, err := s.store.ListWebhooks(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []webhookOut{}
	for _, h := range hooks {
		out = append(out, toWebhookOut(h))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiCreateWebhook(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name   string   `json:"name"`
		URL    string   `json:"url"`
		Format string   `json:"format"`
		Events []string `json:"events"`
		Repos  []string `json:"repos"`
		Secret string   `json:"secret"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	if !validName(in.Name) {
		s.fail(w, badRequest("name is required (letters, digits, '-', '_', '.')"))
		return
	}
	u, err := url.Parse(in.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		s.fail(w, badRequest("url must be an http(s) URL"))
		return
	}
	if in.Format == "" {
		in.Format = "json"
	}
	if !slices.Contains(notify.Formats, in.Format) {
		s.fail(w, badRequest("format must be one of %s", strings.Join(notify.Formats, ", ")))
		return
	}
	events := []string{}
	for _, e := range in.Events {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if e != "*" && e != "remote.*" && !slices.Contains(notify.AllEvents, e) {
			s.fail(w, badRequest("unknown event %q (valid: *, remote.*, %s)", e, strings.Join(notify.AllEvents, ", ")))
			return
		}
		events = append(events, e)
	}
	repos, err := auth.ParseRepoPatterns(strings.Join(in.Repos, ","))
	if err != nil {
		s.fail(w, badRequest("%v", err))
		return
	}
	hook := &store.Webhook{Name: in.Name, URLHost: u.Host, Format: in.Format, Events: events, Repos: repos, Enabled: true}
	if hook.URL, err = s.box.Seal([]byte(in.URL), hook.WebhookAAD("url")); err != nil {
		s.fail(w, err)
		return
	}
	if in.Secret != "" {
		if hook.Secret, err = s.box.Seal([]byte(in.Secret), hook.WebhookAAD("secret")); err != nil {
			s.fail(w, err)
			return
		}
	}
	if err := s.store.CreateWebhook(r.Context(), hook); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toWebhookOut(hook))
}

func (s *Server) webhookFromPath(r *http.Request) (*store.Webhook, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil, badRequest("invalid id")
	}
	return s.store.GetWebhook(r.Context(), id)
}

func (s *Server) apiUpdateWebhook(w http.ResponseWriter, r *http.Request) {
	hook, err := s.webhookFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	if in.Enabled != nil {
		if err := s.store.SetWebhookEnabled(r.Context(), hook.ID, *in.Enabled); err != nil {
			s.fail(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	hook, err := s.webhookFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.DeleteWebhook(r.Context(), hook.ID); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiTestWebhook(w http.ResponseWriter, r *http.Request) {
	hook, err := s.webhookFromPath(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.notify.Test(ctx, hook); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
