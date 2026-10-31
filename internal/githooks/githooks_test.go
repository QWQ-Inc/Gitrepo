package githooks

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func git(t *testing.T, dir string, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_DIR="+dir, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Paths longer than any line buffer must neither hang the hook nor hide an
// oversized blob.
func TestOversizedBlobsWithHugePaths(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "", "init", "-q", "--bare")
	blob := git(t, dir, strings.Repeat("x", 2048), "hash-object", "-w", "--stdin")
	name := strings.Repeat("d", 4000)
	tree := git(t, dir, "100644 blob "+blob+"\tbig.bin\n", "mktree")
	for i := 0; i < 280; i++ { // ~1.1 MB of path
		tree = git(t, dir, "040000 tree "+tree+"\t"+name+"\n", "mktree")
	}
	commit := git(t, dir, "", "commit-tree", tree, "-m", "deep")

	t.Setenv("GIT_DIR", dir)
	done := make(chan struct{})
	var big []string
	var err error
	go func() {
		big, err = oversizedBlobs([]string{commit}, 1024)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("oversizedBlobs hung")
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(big) != 1 || !strings.Contains(big[0], "big.bin (2.0 KiB)") || len(big[0]) > 300 {
		t.Fatalf("oversized blobs: %d entries, first %.80q…", len(big), strings.Join(big, ","))
	}
}
