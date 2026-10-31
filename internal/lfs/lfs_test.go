package lfs

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestParsePointerMatchesGitLFS(t *testing.T) {
	oid := strings.Repeat("c3", 32)
	for name, in := range map[string]string{
		"crlf":      "version https://git-lfs.github.com/spec/v1\r\noid sha256:" + oid + "\r\nsize 7\r\n",
		"hawser":    "version https://hawser.github.com/spec/v1\noid sha256:" + oid + "\nsize 7\n",
		"extension": "version https://git-lfs.github.com/spec/v1\next-0-foo sha256:" + strings.Repeat("0", 64) + "\noid sha256:" + oid + "\nsize 7\n",
	} {
		if p, ok := ParsePointer([]byte(in)); !ok || p.OID != oid || p.Size != 7 {
			t.Errorf("%s: not parsed: %+v %v", name, p, ok)
		}
	}
	for name, in := range map[string]string{
		"keys out of order": "version https://git-lfs.github.com/spec/v1\nsize 7\noid sha256:" + oid + "\n",
		"unknown key":       "version https://git-lfs.github.com/spec/v1\noid sha256:" + oid + "\nsize 7\nzzz 1\n",
		"no final newline":  "version https://git-lfs.github.com/spec/v1\noid sha256:" + oid + "\nsize 7",
		"missing size":      "version https://git-lfs.github.com/spec/v1\noid sha256:" + oid + "\n",
	} {
		if _, ok := ParsePointer([]byte(in)); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTokensAreBoundToTheRepositoryID(t *testing.T) {
	s := NewSigner([]byte("k"))
	oid := strings.Repeat("0", 64)
	tok := s.Sign(RepoKey("acme/demo", 1), oid, "download", time.Minute)
	if s.Verify(tok, RepoKey("acme/demo", 2), oid, "download") == nil {
		t.Fatal("token survived re-creating the repository")
	}
	wide := s.Sign(RepoKey("acme/demo", 1), AnyObject, "upload", time.Minute)
	if s.Verify(wide, RepoKey("acme/demo", 1), oid, "upload") == nil {
		t.Fatal("a repository-wide token authorized an object transfer")
	}
}

func TestBatchLimitsLinksAndLocks(t *testing.T) {
	dir := t.TempDir()
	signer := NewSigner([]byte("k"))
	srv := &Server{Signer: signer, PublicURL: "https://git.example.com", Auth: func(*http.Request, string, bool) error { return ErrUnauthorized }}
	repo := Repo{Slug: "acme/demo", ID: 7, Dir: dir}
	repoTok := "Bearer " + signer.Sign(repo.Key(), AnyObject, "upload", time.Minute)

	var objs []string
	for i := 0; i < maxBatchObjects+1; i++ {
		objs = append(objs, `{"oid":"`+strings.Repeat("a", 64)+`","size":1}`)
	}
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"operation":"download","objects":[`+strings.Join(objs, ",")+`]}`))
	req.Header.Set("Authorization", repoTok)
	rec := httptest.NewRecorder()
	srv.Serve(rec, req, repo, "objects/batch")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized batch: %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "http://attacker.example/x", strings.NewReader(`{"operation":"upload","objects":[{"oid":"`+strings.Repeat("b", 64)+`","size":3}]}`))
	req.Header.Set("Authorization", repoTok)
	rec = httptest.NewRecorder()
	srv.Serve(rec, req, repo, "objects/batch")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"href":"https://git.example.com/acme/demo.git/info/lfs/objects/`) {
		t.Fatalf("links must use the public URL, not the Host header: %d %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{}`))
	req.Header.Set("Authorization", repoTok)
	rec = httptest.NewRecorder()
	srv.Serve(rec, req, repo, "locks/verify")
	if rec.Code != http.StatusOK {
		t.Fatalf("locks/verify with a git-lfs-authenticate token: %d", rec.Code)
	}
}

func TestDamagedObjectIsReplaced(t *testing.T) {
	dir := t.TempDir()
	srv := &Server{Signer: NewSigner([]byte("k")), Auth: func(*http.Request, string, bool) error { return nil }}
	repo := Repo{Slug: "acme/demo", Dir: dir}
	body := []byte("the real content")
	oid := hashHex(body)
	final := ObjectPath(dir, oid)
	os.MkdirAll(filepath.Dir(final), 0o755)
	os.WriteFile(final, nil, 0o644) // e.g. truncated by a crash
	rec := httptest.NewRecorder()
	srv.Serve(rec, httptest.NewRequest(http.MethodPut, "/x", bytes.NewReader(body)), repo, "objects/"+oid)
	if rec.Code != http.StatusOK || !Has(dir, oid, int64(len(body))) {
		t.Fatalf("damaged object not replaced: %d", rec.Code)
	}
}
