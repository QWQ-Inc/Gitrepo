package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/qwq-inc/gitrepo/internal/auth"
	"github.com/qwq-inc/gitrepo/internal/store"
)

func (s *Server) registerSSHAPI(h func(string, http.HandlerFunc)) {
	h("GET /api/ssh", s.apiSSHInfo)
	h("GET /api/ssh-keys", s.apiListSSHKeys)
	h("POST /api/ssh-keys", s.apiCreateSSHKey)
	h("DELETE /api/ssh-keys/{id}", s.apiDeleteSSHKey)
}

func (s *Server) apiSSHInfo(w http.ResponseWriter, r *http.Request) {
	if s.ssh == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":     true,
		"host":        s.sshHost(),
		"host_key":    s.ssh.HostKey(),
		"fingerprint": s.ssh.Fingerprint(),
		"example_url": s.SSHCloneURL("<org>/<name>"),
	})
}

type sshKeyOut struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Fingerprint string     `json:"fingerprint"`
	Type        string     `json:"type"`
	Scopes      []string   `json:"scopes"`
	Repos       []string   `json:"repos,omitempty"`
	CreatedAt   *time.Time `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
}

func toSSHKeyOut(k *store.SSHKey) sshKeyOut {
	typ := ""
	if pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k.PublicKey)); err == nil {
		typ = pk.Type()
	}
	return sshKeyOut{ID: k.ID, Name: k.Name, Fingerprint: k.Fingerprint, Type: typ, Scopes: k.Scopes, Repos: k.Repos,
		CreatedAt: ts(k.CreatedAt), LastUsedAt: ts(k.LastUsedAt)}
}

func (s *Server) apiListSSHKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.ListSSHKeys(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []sshKeyOut{}
	for _, k := range keys {
		out = append(out, toSSHKeyOut(k))
	}
	writeJSON(w, http.StatusOK, out)
}

// acceptableKey rejects key types that are no longer safe.
func acceptableKey(pk ssh.PublicKey) error {
	switch pk.Type() {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
		ssh.KeyAlgoSKED25519, ssh.KeyAlgoSKECDSA256:
		return nil
	case ssh.KeyAlgoRSA:
		if ck, ok := pk.(ssh.CryptoPublicKey); ok {
			if rk, ok := ck.CryptoPublicKey().(interface{ Size() int }); ok && rk.Size()*8 < 2048 {
				return badRequest("RSA keys must have at least 2048 bits")
			}
		}
		return nil
	default:
		return badRequest("unsupported key type %s (use ed25519, ecdsa or rsa ≥ 2048)", pk.Type())
	}
}

func (s *Server) apiCreateSSHKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name      string   `json:"name"`
		PublicKey string   `json:"public_key"`
		Scopes    []string `json:"scopes"`
		Repos     []string `json:"repos"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	if !validName(in.Name) {
		s.fail(w, badRequest("name is required (letters, digits, '-', '_', '.')"))
		return
	}
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(in.PublicKey)))
	if err != nil {
		s.fail(w, badRequest("public_key is not an OpenSSH public key (the contents of e.g. ~/.ssh/id_ed25519.pub)"))
		return
	}
	if err := acceptableKey(pk); err != nil {
		s.fail(w, err)
		return
	}
	scopes, err := auth.ParseScopes(strings.Join(in.Scopes, ","))
	if err != nil {
		s.fail(w, badRequest("%v", err))
		return
	}
	repos, err := auth.ParseRepoPatterns(strings.Join(in.Repos, ","))
	if err != nil {
		s.fail(w, badRequest("%v", err))
		return
	}
	k := &store.SSHKey{
		Name: in.Name, Fingerprint: ssh.FingerprintSHA256(pk),
		PublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk))), Scopes: scopes, Repos: repos,
	}
	if err := s.store.CreateSSHKey(r.Context(), k); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toSSHKeyOut(k))
}

func (s *Server) apiDeleteSSHKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.fail(w, badRequest("invalid id"))
		return
	}
	if err := s.store.DeleteSSHKey(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	if s.ssh != nil {
		s.ssh.Revoke(id)
	}
	w.WriteHeader(http.StatusNoContent)
}
