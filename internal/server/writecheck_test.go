package server_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gr target check --write does every write a sync and a release need in a
// scratch repository on the platform, and deletes it afterwards.
func TestTargetWriteCheck(t *testing.T) {
	_, lfsErr := gitCmd("", "lfs", "version")
	e := newEnv(t)
	fake := newFakePlatforms(t)
	for _, tg := range []map[string]any{
		{"name": "github", "provider": "github", "namespace": "acme", "base_url": fake.srv.URL + "/gh", "credential": map[string]any{"kind": "token", "secret": ghToken}},
		{"name": "gitlab", "provider": "gitlab", "namespace": "acme-group", "base_url": fake.srv.URL + "/gl", "credential": map[string]any{"kind": "token", "secret": glToken}},
		{"name": "forgejo", "provider": "gitea", "namespace": "acme", "base_url": fake.srv.URL + "/gt", "credential": map[string]any{"kind": "token", "secret": gtToken}},
		{"name": "gitee", "provider": "gitee", "namespace": "acme", "base_url": fake.srv.URL + "/ge", "credential": map[string]any{"kind": "token", "secret": geToken}},
		{"name": "plain", "provider": "generic", "url_template": e.remoteURL + "/a/{name}.git", "credential": map[string]any{"kind": "basic", "username": userA, "secret": passA}},
	} {
		e.do(http.MethodPost, "/api/targets", tg, nil)
	}
	type step struct {
		Name, State, Detail string
	}
	type result struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
		Steps    []step `json:"steps"`
		Leftover string `json:"leftover"`
	}
	check := func(name string) result {
		var res result
		e.do(http.MethodPost, "/api/targets/"+name+"/check", map[string]any{"write": true}, &res)
		return res
	}
	platformDir := map[string]string{"github": "gh/acme", "gitlab": "gl/acme-group", "forgejo": "gt/acme", "gitee": "ge/acme"}
	for _, name := range []string{"github", "gitlab", "forgejo", "gitee"} {
		res := check(name)
		states := map[string]string{}
		for _, st := range res.Steps {
			states[st.Name] = st.State
		}
		want := []string{"create repository", "push", "default branch", "visibility", "release", "delete repository"}
		if lfsErr == nil {
			want = append(want, "Git LFS upload")
		}
		for _, w := range want {
			if states[w] != "ok" {
				t.Errorf("%s: step %q is %q: %+v", name, w, states[w], res.Steps)
			}
		}
		if !res.OK || res.Leftover != "" || res.Error != "" {
			t.Errorf("%s: %+v", name, res)
		}
		// Nothing is left on the platform.
		left, _ := filepath.Glob(filepath.Join(fake.dir, platformDir[name], "gitrepo-check-*"))
		if len(left) > 0 {
			t.Errorf("%s: scratch repository left behind: %v", name, left)
		}
	}
	if lfsErr == nil && !calledPrefix(fake, "lfs upload gh acme/gitrepo-check-") {
		t.Error("no LFS object was uploaded to the platform")
	}

	// Without the permission to delete repositories: the check still
	// passes for mirroring, and names the repository to delete by hand.
	fake.mu.Lock()
	fake.denyDelete = true
	fake.mu.Unlock()
	res := check("github")
	if res.OK || !strings.HasPrefix(res.Leftover, "acme/gitrepo-check-") {
		t.Fatalf("delete refused: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(fake.dir, "gh", res.Leftover+".git")); err != nil {
		t.Fatalf("the reported leftover does not exist: %v", err)
	}
	var page auditPage
	e.do(http.MethodGet, "/api/audit?action=targets", nil, &page)
	if page.find(func(r auditRow) bool { return strings.Contains(r.Detail, "left on the platform") }) == nil {
		t.Fatalf("the leftover is not in the audit log: %+v", page.Entries)
	}

	// Generic targets have no API: nothing can be checked this way.
	res = check("plain")
	if res.OK || !strings.Contains(res.Error, "no API") || len(res.Steps) != 0 {
		t.Fatalf("generic target: %+v", res)
	}
}

func calledPrefix(f *fakePlatforms, prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}
