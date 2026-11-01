package provider

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/store"
)

// Contract tests run the adapters' read paths against the real platforms'
// public APIs, anonymously, to catch differences between the fakes used in
// the other tests and reality (response shapes, ID types, status codes,
// tag escaping). They need network access, so they only run with
// GITREPO_CONTRACT=1:
//
//	GITREPO_CONTRACT=1 go test ./internal/provider -run Contract -v
//
// GITREPO_CONTRACT_GITHUB=owner/repo adds GitHub (any repository the
// environment may read; only reads are made).

func contract(t *testing.T) context.Context {
	if os.Getenv("GITREPO_CONTRACT") != "1" {
		t.Skip("set GITREPO_CONTRACT=1 to run against the real platforms")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// latestTag reads the newest release tag of a public project.
func latestTag(t *testing.T, ctx context.Context, u string) string {
	t.Helper()
	var list []struct {
		TagName string `json:"tag_name"`
	}
	if _, err := doJSON(ctx, http.MethodGet, u, nil, nil, &list); err != nil || len(list) == 0 {
		t.Fatalf("list releases %s: %v %v", u, list, err)
	}
	return list[0].TagName
}

func TestContractGitLab(t *testing.T) {
	ctx := contract(t)
	g := &GitLab{}
	tg := &store.Target{Provider: "gitlab", BaseURL: "https://gitlab.com", CredKind: "token"}
	spec := RepoSpec{Namespace: "gitlab-org", Name: "gitlab-runner"}
	tag := latestTag(t, ctx, "https://gitlab.com/api/v4/projects/gitlab-org%2Fgitlab-runner/releases?per_page=1")
	rr, err := g.GetRelease(ctx, tg, "", spec, tag)
	if err != nil || rr == nil {
		t.Fatalf("GetRelease(%s): %+v %v", tag, rr, err)
	}
	if rr.ID != tag || !strings.HasPrefix(rr.Key, tag+"@20") {
		t.Fatalf("GitLab release identity: ID %q, Key %q", rr.ID, rr.Key)
	}
	if len(rr.Assets) == 0 || rr.Assets[0].ID == "" || rr.Assets[0].Name == "" {
		t.Fatalf("GitLab asset links: %+v", rr.Assets)
	}
	if rr, err := g.GetRelease(ctx, tg, "", spec, "v0.0.0-gitrepo-contract"); rr != nil || err != nil {
		t.Fatalf("missing release: %+v %v", rr, err)
	}
}

func TestContractForgejo(t *testing.T) {
	ctx := contract(t)
	g := &Gitea{}
	tg := &store.Target{Provider: "gitea", BaseURL: "https://codeberg.org", CredKind: "token"}
	spec := RepoSpec{Namespace: "forgejo", Name: "forgejo"}
	tag := latestTag(t, ctx, "https://codeberg.org/api/v1/repos/forgejo/forgejo/releases?limit=1")
	rr, err := g.GetRelease(ctx, tg, "", spec, tag)
	if err != nil || rr == nil {
		t.Fatalf("GetRelease(%s): %+v %v", tag, rr, err)
	}
	if rr.ID == "" || rr.ID != rr.Key || strings.Trim(rr.ID, "0123456789") != "" {
		t.Fatalf("Forgejo release ID: %q / %q", rr.ID, rr.Key)
	}
	if len(rr.Assets) == 0 || rr.Assets[0].ID == "" || rr.Assets[0].Size <= 0 {
		t.Fatalf("Forgejo assets: %+v", rr.Assets)
	}
	if rr, err := g.GetRelease(ctx, tg, "", spec, "v0.0.0-gitrepo-contract"); rr != nil || err != nil {
		t.Fatalf("missing release: %+v %v", rr, err)
	}
	// A tag with "/" is one escaped path segment.
	if rr, err := g.GetRelease(ctx, tg, "", spec, "gitrepo/contract"); rr != nil || err != nil {
		t.Fatalf("slashed tag: %+v %v", rr, err)
	}
}

func TestContractGitee(t *testing.T) {
	ctx := contract(t)
	g := &Gitee{}
	tg := &store.Target{Provider: "gitee", BaseURL: "https://gitee.com", CredKind: "token"}
	spec := RepoSpec{Namespace: "openharmony", Name: "docs"}
	rr, err := g.GetRelease(ctx, tg, "", spec, "OpenHarmony-1.0")
	if err != nil || rr == nil {
		t.Fatalf("GetRelease: %+v %v", rr, err)
	}
	if rr.ID == "" || rr.ID != rr.Key || strings.Trim(rr.ID, "0123456789") != "" {
		t.Fatalf("Gitee release ID: %q", rr.ID)
	}
	for _, a := range rr.Assets {
		if a.ID == "" {
			t.Fatalf("Gitee attach_files without IDs (source archives must not be listed): %+v", rr.Assets)
		}
	}
	// Gitee answers null with status 200 for a tag without a release.
	if rr, err := g.GetRelease(ctx, tg, "", spec, "v0.0.0-gitrepo-contract"); rr != nil || err != nil {
		t.Fatalf("missing release: %+v %v", rr, err)
	}
	if rr, err := g.GetRelease(ctx, tg, "", spec, "gitrepo/contract"); rr != nil || err != nil {
		t.Fatalf("slashed tag: %+v %v", rr, err)
	}
}

func TestContractGitHub(t *testing.T) {
	ctx := contract(t)
	repo := os.Getenv("GITREPO_CONTRACT_GITHUB")
	if repo == "" {
		t.Skip("set GITREPO_CONTRACT_GITHUB=owner/repo to include GitHub")
	}
	owner, name, _ := strings.Cut(repo, "/")
	g := &GitHub{}
	tg := &store.Target{Provider: "github", Namespace: owner, CredKind: "token"}
	spec := RepoSpec{Namespace: owner, Name: name}
	secret := os.Getenv("GITHUB_TOKEN")
	if rr, err := g.GetRelease(ctx, tg, secret, spec, "v0.0.0-gitrepo-contract"); rr != nil || err != nil {
		t.Fatalf("missing release: %+v %v", rr, err)
	}
	if rr, err := g.GetRelease(ctx, tg, secret, spec, "gitrepo/contract"); rr != nil || err != nil {
		t.Fatalf("slashed tag: %+v %v", rr, err)
	}
	// The repository lookup EnsureRepo starts with.
	var info struct {
		DefaultBranch string `json:"default_branch"`
	}
	if _, err := doJSON(ctx, http.MethodGet, g.api(tg)+"/repos/"+owner+"/"+name, g.headers(secret), nil, &info); err != nil || info.DefaultBranch == "" {
		t.Fatalf("repository lookup: %+v %v", info, err)
	}
}
