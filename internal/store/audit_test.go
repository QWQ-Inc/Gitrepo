package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestAuditStore(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	old := FromTime(time.Now().Add(-48 * time.Hour))
	for _, e := range []*AuditEntry{
		{At: old, Actor: "admin", Via: "api", Action: "DELETE /api/tokens/{id}", Status: 204},
		{Actor: "dev", Via: "http", Action: "git push", Repo: "acme/app", Detail: "main created"},
		{Actor: "admin", Via: "api", Action: "POST /api/repos/{org}/{name}/backups/purge", Repo: "acme/app", Status: 200},
	} {
		if err := s.AddAudit(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	long := &AuditEntry{Actor: "dev", Via: "http", Action: "git push", Detail: string(make([]byte, 10000))}
	if err := s.AddAudit(ctx, long); err != nil || len(long.Detail) != maxAuditDetail {
		t.Fatalf("long detail: %v %d", err, len(long.Detail))
	}
	arrows := &AuditEntry{Actor: "dev", Via: "http", Action: "git push", Detail: strings.Repeat("→", 2000)}
	if err := s.AddAudit(ctx, arrows); err != nil || !utf8.ValidString(arrows.Detail) || len(arrows.Detail) > maxAuditDetail {
		t.Fatalf("multi-byte detail cut badly: %v %d", err, len(arrows.Detail))
	}

	count := func(f AuditFilter) int {
		l, err := s.ListAudit(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		return len(l)
	}
	if n := count(AuditFilter{Action: "purge"}); n != 1 {
		t.Errorf("action substring: %d", n)
	}
	if n := count(AuditFilter{Action: "%"}); n != 0 {
		t.Errorf("%% must match literally: %d", n)
	}
	if n := count(AuditFilter{Action: "_"}); n != 0 {
		t.Errorf("_ must match literally: %d", n)
	}
	if n := count(AuditFilter{Repo: "acme/app", Actor: "admin"}); n != 1 {
		t.Errorf("repo+actor: %d", n)
	}
	if n, err := s.PruneAudit(ctx, FromTime(time.Now().Add(-24*time.Hour))); err != nil || n != 1 {
		t.Fatalf("prune: %d %v", n, err)
	}
	if n := count(AuditFilter{}); n != 4 {
		t.Errorf("after prune: %d", n)
	}
}
