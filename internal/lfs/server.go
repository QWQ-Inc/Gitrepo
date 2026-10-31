package lfs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Errors returned by Server.Auth.
var (
	ErrUnauthorized = errors.New("authentication required")
	ErrForbidden    = errors.New("access denied")
)

const mediaType = "application/vnd.git-lfs+json"

// actionTTL is how long transfer hrefs stay valid.
const actionTTL = time.Hour

// Repo identifies the repository a request is for.
type Repo struct {
	Slug string
	Dir  string
}

// Server serves <repo>.git/info/lfs/*.
type Server struct {
	Signer *Signer
	// Auth authorizes a request carrying user credentials for read or write
	// access. It returns ErrUnauthorized, ErrForbidden or nil.
	Auth func(r *http.Request, slug string, write bool) error
	// MaxObjectSize rejects larger uploads (0 = unlimited).
	MaxObjectSize int64
	Log           *slog.Logger
}

type batchObject struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

type batchRequest struct {
	Operation string        `json:"operation"`
	Transfers []string      `json:"transfers"`
	Objects   []batchObject `json:"objects"`
	HashAlgo  string        `json:"hash_algo"`
}

type action struct {
	Href      string            `json:"href"`
	Header    map[string]string `json:"header,omitempty"`
	ExpiresIn int               `json:"expires_in,omitempty"`
}

type objError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type batchResponseObject struct {
	OID           string             `json:"oid"`
	Size          int64              `json:"size"`
	Authenticated bool               `json:"authenticated,omitempty"`
	Actions       map[string]*action `json:"actions,omitempty"`
	Error         *objError          `json:"error,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", mediaType)
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("LFS-Authenticate", `Basic realm="gitrepo"`)
		w.Header().Set("WWW-Authenticate", `Basic realm="gitrepo"`)
	}
	writeJSON(w, status, map[string]string{"message": "gitrepo: " + msg})
}

func (s *Server) authErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUnauthorized):
		writeErr(w, http.StatusUnauthorized, "credentials required")
	case errors.Is(err, ErrForbidden):
		writeErr(w, http.StatusForbidden, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "authorization failed")
	}
}

// baseURL reconstructs the externally visible URL of the repository from
// the request, so hrefs work behind a reverse proxy that keeps the Host.
func baseURL(r *http.Request, slug string) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/" + slug + ".git/info/lfs"
}

// bearer returns an action token from the Authorization header, if any.
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if v, ok := strings.CutPrefix(h, "Bearer "); ok && IsToken(v) {
		return v
	}
	return ""
}

// Serve handles one request; rest is the path below info/lfs/.
func (s *Server) Serve(w http.ResponseWriter, r *http.Request, repo Repo, rest string) {
	switch {
	case rest == "objects/batch" && r.Method == http.MethodPost:
		s.batch(w, r, repo)
	case strings.HasPrefix(rest, "objects/") && ValidOID(strings.TrimPrefix(rest, "objects/")):
		oid := strings.TrimPrefix(rest, "objects/")
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			s.download(w, r, repo, oid)
		case http.MethodPut:
			s.upload(w, r, repo, oid)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case rest == "verify" && r.Method == http.MethodPost:
		s.verify(w, r, repo)
	case rest == "locks/verify" && r.Method == http.MethodPost:
		// Locking is not supported; report no locks so pushes proceed
		// without the client warning on every push.
		if err := s.Auth(r, repo.Slug, true); err != nil {
			s.authErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ours": []any{}, "theirs": []any{}})
	case rest == "locks" && r.Method == http.MethodGet:
		if err := s.Auth(r, repo.Slug, false); err != nil {
			s.authErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"locks": []any{}})
	case strings.HasPrefix(rest, "locks"):
		writeErr(w, http.StatusNotImplemented, "file locking is not supported")
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) batch(w http.ResponseWriter, r *http.Request, repo Repo) {
	var req batchRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "invalid batch request")
		return
	}
	if req.Operation != "upload" && req.Operation != "download" {
		writeErr(w, http.StatusUnprocessableEntity, "operation must be upload or download")
		return
	}
	if req.HashAlgo != "" && req.HashAlgo != "sha256" {
		writeErr(w, http.StatusConflict, "only sha256 is supported")
		return
	}
	if len(req.Transfers) > 0 && !contains(req.Transfers, "basic") {
		writeErr(w, http.StatusUnprocessableEntity, "only the basic transfer adapter is supported")
		return
	}
	if err := s.Auth(r, repo.Slug, req.Operation == "upload"); err != nil {
		s.authErr(w, err)
		return
	}
	base := baseURL(r, repo.Slug)
	out := make([]batchResponseObject, 0, len(req.Objects))
	for _, o := range req.Objects {
		ro := batchResponseObject{OID: o.OID, Size: o.Size}
		switch {
		case !ValidOID(o.OID) || o.Size < 0:
			ro.Error = &objError{Code: http.StatusUnprocessableEntity, Message: "invalid object"}
		case req.Operation == "download":
			if !Has(repo.Dir, o.OID, o.Size) {
				ro.Error = &objError{Code: http.StatusNotFound, Message: "object does not exist on the hub"}
				break
			}
			ro.Authenticated = true
			ro.Actions = map[string]*action{"download": s.action(base+"/objects/"+o.OID, repo.Slug, o.OID, "download")}
		default: // upload
			if s.MaxObjectSize > 0 && o.Size > s.MaxObjectSize {
				ro.Error = &objError{Code: http.StatusUnprocessableEntity, Message: fmt.Sprintf("object is larger than the %d-byte limit", s.MaxObjectSize)}
				break
			}
			if Has(repo.Dir, o.OID, o.Size) {
				break // already stored: no actions means nothing to upload
			}
			ro.Authenticated = true
			ro.Actions = map[string]*action{
				"upload": s.action(base+"/objects/"+o.OID, repo.Slug, o.OID, "upload"),
				"verify": s.action(base+"/verify", repo.Slug, o.OID, "upload"),
			}
		}
		out = append(out, ro)
	}
	writeJSON(w, http.StatusOK, map[string]any{"transfer": "basic", "objects": out, "hash_algo": "sha256"})
}

func (s *Server) action(href, slug, oid, op string) *action {
	return &action{
		Href:      href,
		Header:    map[string]string{"Authorization": "Bearer " + s.Signer.Sign(slug, oid, op, actionTTL)},
		ExpiresIn: int(actionTTL / time.Second),
	}
}

// authorizeObject accepts an action token for this object or user
// credentials with the needed scope.
func (s *Server) authorizeObject(w http.ResponseWriter, r *http.Request, repo Repo, oid, op string) bool {
	if tok := bearer(r); tok != "" {
		if err := s.Signer.Verify(tok, repo.Slug, oid, op); err != nil {
			writeErr(w, http.StatusUnauthorized, err.Error())
			return false
		}
		return true
	}
	if err := s.Auth(r, repo.Slug, op == "upload"); err != nil {
		s.authErr(w, err)
		return false
	}
	return true
}

func (s *Server) download(w http.ResponseWriter, r *http.Request, repo Repo, oid string) {
	if !s.authorizeObject(w, r, repo, oid, "download") {
		return
	}
	f, err := os.Open(ObjectPath(repo.Dir, oid))
	if err != nil {
		writeErr(w, http.StatusNotFound, "object does not exist on the hub")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot read object")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, oid, fi.ModTime(), f)
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request, repo Repo, oid string) {
	if !s.authorizeObject(w, r, repo, oid, "upload") {
		return
	}
	if s.MaxObjectSize > 0 && r.ContentLength > s.MaxObjectSize {
		writeErr(w, http.StatusRequestEntityTooLarge, "object is too large")
		return
	}
	final := ObjectPath(repo.Dir, oid)
	if _, err := os.Stat(final); err == nil {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		return
	}
	tmpDir := filepath.Join(repo.Dir, "lfs", "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot store object")
		return
	}
	tmp, err := os.CreateTemp(tmpDir, oid+"-*")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot store object")
		return
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	h := sha256.New()
	var body io.Reader = r.Body
	if s.MaxObjectSize > 0 {
		body = io.LimitReader(r.Body, s.MaxObjectSize+1)
	}
	n, err := io.Copy(io.MultiWriter(tmp, h), body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "upload interrupted")
		return
	}
	if s.MaxObjectSize > 0 && n > s.MaxObjectSize {
		writeErr(w, http.StatusRequestEntityTooLarge, "object is too large")
		return
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != oid {
		writeErr(w, http.StatusUnprocessableEntity, "content does not match the object id")
		return
	}
	if r.ContentLength >= 0 && n != r.ContentLength {
		writeErr(w, http.StatusBadRequest, "size mismatch")
		return
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot store object")
		return
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot store object")
		return
	}
	if s.Log != nil {
		s.Log.Debug("lfs object stored", "repo", repo.Slug, "oid", oid, "size", n)
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) verify(w http.ResponseWriter, r *http.Request, repo Repo) {
	var o batchObject
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&o); err != nil || !ValidOID(o.OID) {
		writeErr(w, http.StatusUnprocessableEntity, "invalid verify request")
		return
	}
	if !s.authorizeObject(w, r, repo, o.OID, "upload") {
		return
	}
	if !Has(repo.Dir, o.OID, o.Size) {
		writeErr(w, http.StatusNotFound, "object is missing or has the wrong size")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
