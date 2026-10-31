package lfs

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParsePointer(t *testing.T) {
	oid := strings.Repeat("a1", 32)
	p, ok := ParsePointer([]byte("version https://git-lfs.github.com/spec/v1\noid sha256:" + oid + "\nsize 42\n"))
	if !ok || p.OID != oid || p.Size != 42 {
		t.Fatalf("ParsePointer = %+v %v", p, ok)
	}
	for _, bad := range []string{
		"hello world",
		"version https://git-lfs.github.com/spec/v1\noid sha256:xyz\nsize 1\n",
		"version https://git-lfs.github.com/spec/v1\noid sha256:" + oid + "\nsize -1\n",
	} {
		if _, ok := ParsePointer([]byte(bad)); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestSigner(t *testing.T) {
	s := NewSigner([]byte("k"))
	oid := strings.Repeat("0", 64)
	tok := s.Sign("acme/demo", oid, "download", time.Minute)
	if err := s.Verify(tok, "acme/demo", oid, "download"); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"other repo":    s.Verify(tok, "acme/other", oid, "download"),
		"other object":  s.Verify(tok, "acme/demo", strings.Repeat("1", 64), "download"),
		"wrong op":      s.Verify(tok, "acme/demo", oid, "upload"),
		"other key":     NewSigner([]byte("x")).Verify(tok, "acme/demo", oid, "download"),
		"tampered body": s.Verify(strings.Replace(tok, "lfs1.e", "lfs1.f", 1), "acme/demo", oid, "download"),
	} {
		if err == nil {
			t.Errorf("%s: token accepted", name)
		}
	}
	up := s.Sign("acme/demo", oid, "upload", time.Minute)
	if s.Verify(up, "acme/demo", oid, "download") != nil {
		t.Error("an upload token should also allow verify/download of the same object")
	}
	s.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if s.Verify(tok, "acme/demo", oid, "download") == nil {
		t.Error("expired token accepted")
	}
}

func TestUploadChecksContent(t *testing.T) {
	dir := t.TempDir()
	srv := &Server{Signer: NewSigner([]byte("k")), Auth: func(*http.Request, string, bool) error { return nil }}
	repo := Repo{Slug: "acme/demo", Dir: dir}
	body := []byte("hello lfs")
	oid := hashHex(body)

	put := func(id string, b []byte) int {
		rec := httptest.NewRecorder()
		srv.Serve(rec, httptest.NewRequest(http.MethodPut, "/x", bytes.NewReader(b)), repo, "objects/"+id)
		return rec.Code
	}
	if code := put(oid, []byte("tampered")); code != http.StatusUnprocessableEntity {
		t.Fatalf("mismatched content: %d", code)
	}
	if Has(dir, oid, -1) {
		t.Fatal("mismatched content was stored")
	}
	if code := put(oid, body); code != http.StatusOK || !Has(dir, oid, int64(len(body))) {
		t.Fatalf("upload: %d", code)
	}
	if entries, _ := os.ReadDir(dir + "/lfs/tmp"); len(entries) != 0 {
		t.Fatalf("temporary files left behind: %d", len(entries))
	}
	rec := httptest.NewRecorder()
	srv.Serve(rec, httptest.NewRequest(http.MethodGet, "/x", nil), repo, "objects/"+oid)
	if rec.Code != http.StatusOK || rec.Body.String() != string(body) {
		t.Fatalf("download: %d %q", rec.Code, rec.Body.String())
	}
	srv.MaxObjectSize = 4
	if code := put(hashHex([]byte("too big")), []byte("too big")); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload: %d", code)
	}
}
