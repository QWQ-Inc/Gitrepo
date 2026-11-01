package server_test

import (
	"bytes"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/client"
)

// The edge's negative cache is keyed by OID alone, but the hub answers 404
// for a known OID asked for with the wrong size (lfs.Has checks the size).
// Anyone who can read a repository - anonymous users of a public one - can
// ask the edge for an object with a made-up size and make the edge refuse
// that object to everyone for 30 seconds (repeatable indefinitely): batch
// answers say "object does not exist on the hub" and even a download
// through a link handed out earlier fails.
func TestEdgeLFSMissCacheIgnoresWrongSize(t *testing.T) {
	e := newEnv(t)
	var edgeTok, dev client.Token
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "edge", "scopes": []string{"edge"}}, &edgeTok)
	e.do(http.MethodPost, "/api/tokens", map[string]any{"name": "dev", "scopes": []string{"create"}}, &dev)
	e.do(http.MethodPost, "/api/repos", map[string]any{"slug": "acme/pub", "visibility": "public"}, nil)
	work := filepath.Join(t.TempDir(), "work")
	e.run("", "init", "-q", "-b", "main", work)
	e.commit(work, "a.txt", "1\n")
	e.run(work, "push", "-q", e.hubURL(dev.Token, "acme/pub"), "main")
	content := []byte("a large build artifact\n")
	oid := lfsOID(content)
	putLFS(t, e.hubRepo("acme/pub"), oid, content)

	n := startEdgeNode(t, e, edgeTok.Token, filepath.Join(t.TempDir(), "edge"), "", "")
	eventually(t, "edge ready", 15*time.Second, func() bool {
		_, err := lsRefs(n.url("", "acme/pub"))
		return err == nil
	})

	// Control: the hub itself serves the object to an anonymous batch.
	_, hub := lfsBatch(t, e.hubURL("", "acme/pub"), "", []map[string]any{{"oid": oid, "size": len(content)}})
	if len(hub) != 1 || hub[0].Actions["download"] == nil {
		t.Fatalf("control: hub batch %+v", hub)
	}

	// An anonymous client asks the edge for the object with a wrong size.
	_, bad := lfsBatch(t, n.url("", "acme/pub"), "", []map[string]any{{"oid": oid, "size": 1}})
	t.Logf("anonymous batch with size 1: %+v", bad)

	// A legitimate client with the correct size.
	_, got := lfsBatch(t, n.url("", "acme/pub"), dev.Token, []map[string]any{{"oid": oid, "size": len(content)}})
	t.Logf("batch with the correct size right after: %+v", got)
	if len(got) != 1 || got[0].Actions["download"] == nil {
		code := 0
		if len(got) == 1 && got[0].Error != nil {
			code = got[0].Error.Code
		}
		t.Errorf("BUG: the edge refuses an object the hub has (error code %d) after one anonymous request with a wrong size", code)
	}
	resp, err := http.Get(n.url("", "acme/pub") + "/info/lfs/objects/" + oid)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("direct download from the edge: %d %q", resp.StatusCode, b)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(b, content) {
		t.Errorf("BUG: direct download of the object from the edge fails: %d", resp.StatusCode)
	}
}
