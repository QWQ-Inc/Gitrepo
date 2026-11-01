package server

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// edgeHandler serves an edge node: Git reads, archives and LFS from the
// local copy, pushes redirected to the hub. The admin API, console and
// releases stay on the hub.
func (s *Server) edgeHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /archive/{org}/{name}/{file...}", func(w http.ResponseWriter, r *http.Request) {
		s.gitsrv.ServeArchive(w, r, r.PathValue("org"), r.PathValue("name"), r.PathValue("file"))
	})
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.requireAdmin(s.handleMetrics))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Gitrepo edge node of %s: clone and fetch here; pushes and the console are on the hub.\n", s.cfg.Edge.Upstream)
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, errors.New("this is a read-only edge node; use the hub's API at "+s.cfg.Edge.Upstream))
	})
	mux.HandleFunc("/ui/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, s.cfg.Edge.Upstream+r.URL.RequestURI(), http.StatusFound)
	})
	mux.Handle("/", s.gitsrv)
	return logRequests(s.log, mux)
}

// edgeMetrics describes replication for /metrics on an edge node.
func (s *Server) edgeMetrics(b *strings.Builder) {
	st := s.edge.Status()
	ready := 0
	if st.Ready {
		ready = 1
	}
	fmt.Fprintf(b, "# HELP gitrepo_edge_ready Whether the edge has loaded its copy of the hub.\n# TYPE gitrepo_edge_ready gauge\ngitrepo_edge_ready %d\n", ready)
	last := int64(0)
	if !st.LastPoll.IsZero() {
		last = st.LastPoll.Unix()
	}
	fmt.Fprintf(b, "# HELP gitrepo_edge_last_update_timestamp_seconds Last time the edge applied the hub's state.\n# TYPE gitrepo_edge_last_update_timestamp_seconds gauge\ngitrepo_edge_last_update_timestamp_seconds %d\n", last)
	fmt.Fprintf(b, "# HELP gitrepo_edge_pending_fetches Repositories being fetched from the hub.\n# TYPE gitrepo_edge_pending_fetches gauge\ngitrepo_edge_pending_fetches %d\n", st.Pending)
	up := 1
	if st.LastError != "" {
		up = 0
	}
	fmt.Fprintf(b, "# HELP gitrepo_edge_upstream_ok Whether the last exchange with the hub succeeded.\n# TYPE gitrepo_edge_upstream_ok gauge\ngitrepo_edge_upstream_ok %d\n", up)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
