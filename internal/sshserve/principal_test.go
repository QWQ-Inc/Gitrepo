package sshserve

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/qwq-inc/gitrepo/internal/store"
)

// An open connection stays bound to the key it authenticated with, even if
// that key's row ID is later reused by another key.
func TestPrincipalChecksFingerprint(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	b := &store.SSHKey{Name: "b", Fingerprint: "SHA256:bbb", PublicKey: "ssh-ed25519 AAAAb", Scopes: []string{"read"}, Repos: []string{"acme/open"}}
	if err := st.CreateSSHKey(ctx, b); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st}
	ref := keyRef{id: b.ID, fp: b.Fingerprint}
	if _, err := s.principal(ref); err != nil {
		t.Fatal(err)
	}
	st.DeleteSSHKey(ctx, b.ID)
	c := &store.SSHKey{Name: "c", Fingerprint: "SHA256:ccc", PublicKey: "ssh-ed25519 AAAAc", Scopes: []string{"admin"}}
	if err := st.CreateSSHKey(ctx, c); err != nil {
		t.Fatal(err)
	}
	if c.ID != b.ID {
		t.Skipf("row id not reused (%d vs %d); nothing to check", c.ID, b.ID)
	}
	if p, err := s.principal(ref); err == nil {
		t.Fatalf("a deleted key's connection got the new key's rights: %+v", p)
	}
}
