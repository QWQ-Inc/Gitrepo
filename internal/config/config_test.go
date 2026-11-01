package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gitrepo.yaml")
	os.WriteFile(path, []byte(`version: 1
listen: ":9000"
public_url: "https://git.example.com/"
data_dir: "`+dir+`/data"
sync:
  workers: 8
  patrol_interval: 5m
  backup_retention: 7d
policy:
  max_file_size: 50MiB
`), 0o644)
	t.Setenv("GITREPO_LISTEN", ":9100")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":9100" {
		t.Errorf("env override: listen = %s", cfg.Listen)
	}
	if cfg.PublicURL != "https://git.example.com" {
		t.Errorf("public_url = %s", cfg.PublicURL)
	}
	if cfg.Sync.Workers != 8 || cfg.Sync.PatrolInterval.D() != 5*time.Minute || cfg.Sync.BackupRetention.D() != 7*24*time.Hour {
		t.Errorf("sync = %+v", cfg.Sync)
	}
	if cfg.Sync.PushTimeout.D() != 10*time.Minute {
		t.Errorf("default push timeout lost: %v", cfg.Sync.PushTimeout.D())
	}
	if cfg.Policy.MaxFileSize != 50<<20 {
		t.Errorf("max_file_size = %d", cfg.Policy.MaxFileSize)
	}
}

func TestLoadRejectsBadVersions(t *testing.T) {
	dir := t.TempDir()
	for _, body := range []string{"listen: x\n", "version: 99\n"} {
		path := filepath.Join(dir, "c.yaml")
		os.WriteFile(path, []byte(body), 0o644)
		if _, err := Load(path); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
}

func TestParseByteSize(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "": 0, "100MiB": 100 << 20, "100MB": 100e6, "1.5GiB": 3 << 29, "512": 512, "2K": 2048} {
		got, err := ParseByteSize(in)
		if err != nil || got != want {
			t.Errorf("ParseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := ParseByteSize("lots"); err == nil {
		t.Error("accepted garbage")
	}
}

func TestTrustedProxies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	os.WriteFile(path, []byte("version: 1\ntrusted_proxies: [\"10.0.0.0/8\", \"192.168.1.10\", \"::1\"]\naudit:\n  retention: 90d\n"), 0o644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.TrustedProxyPrefixes()
	if len(got) != 3 || got[0].String() != "10.0.0.0/8" || got[1].String() != "192.168.1.10/32" || got[2].String() != "::1/128" {
		t.Errorf("trusted proxies = %v", got)
	}
	if cfg.Audit.Retention.D() != 90*24*time.Hour {
		t.Errorf("audit retention = %v", cfg.Audit.Retention.D())
	}
	os.WriteFile(path, []byte("version: 1\ntrusted_proxies: [\"10.0.0.0/33\"]\n"), 0o644)
	if _, err := Load(path); err == nil {
		t.Error("accepted an invalid CIDR")
	}
}

func TestExampleConfigLoads(t *testing.T) {
	if _, err := Load("../../deploy/gitrepo.example.yaml"); err != nil {
		t.Fatalf("deploy/gitrepo.example.yaml: %v", err)
	}
}
