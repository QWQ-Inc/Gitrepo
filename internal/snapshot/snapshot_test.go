package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadManifestRejectsUnsafeEntries(t *testing.T) {
	for _, body := range []string{
		`{"format":1,"repos":[{"slug":"../../etc/x"}]}`,
		`{"format":1,"repos":[{"slug":"acme/app","bundle":"../../secret.bundle"}]}`,
		`{"format":1,"repos":[{"slug":"acme/a/b"}]}`,
		`{"format":2,"repos":[]}`,
	} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(body), 0o600)
		if _, err := ReadManifest(dir); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
	if _, err := ReadManifest(t.TempDir()); err == nil || !strings.Contains(err.Error(), "not a complete snapshot") {
		t.Errorf("missing manifest: %v", err)
	}
}
