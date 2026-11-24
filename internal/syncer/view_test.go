package syncer

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseView(t *testing.T) {
	v, err := ParseView([]string{"api:main", "refs/tags/api-v*:refs/tags/v*", " docs "}, "/services/api/")
	if err != nil {
		t.Fatal(err)
	}
	want := []RefMap{
		{"refs/heads/api", "refs/heads/main"},
		{"refs/tags/api-v*", "refs/tags/v*"},
		{"refs/heads/docs", "refs/heads/docs"},
	}
	if !reflect.DeepEqual(v.Maps, want) || v.Subdir != "services/api" {
		t.Fatalf("%+v", v)
	}
	for _, bad := range [][]string{
		{"main:refs/tags/main"},             // branch → tag
		{"refs/notes/x:refs/notes/x"},       // not a branch or tag
		{"a*:b"},                            // wildcard on one side only
		{"a*b*:c*d*"},                       // two wildcards
		{"a..b"}, {"x:y z"}, {"-x"}, {"a/"}, // bad names
	} {
		if _, err := ParseView(bad, ""); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	for _, sub := range []string{"../x", "a/../b", "a/./b", ".git", "x/.git/y", "a\\b"} {
		if _, err := ParseView(nil, sub); err == nil {
			t.Errorf("accepted subdirectory %q", sub)
		}
	}
	if v, _ := ParseView(nil, ""); !v.IsZero() {
		t.Error("an empty view must be the default")
	}
}

func TestViewApply(t *testing.T) {
	hub := map[string]string{
		"refs/heads/main": "m", "refs/heads/api": "a", "refs/heads/web": "w",
		"refs/tags/api-v1.0": "t1", "refs/tags/web-v2.0": "t2", "refs/tags/v0.1": "t0",
	}
	apply := func(maps []string, sub string) map[string]string {
		t.Helper()
		v, err := ParseView(maps, sub)
		if err != nil {
			t.Fatal(err)
		}
		got, _, err := v.Apply(hub)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	// Default: everything as it is.
	if got := apply(nil, ""); !reflect.DeepEqual(got, hub) {
		t.Errorf("default view: %v", got)
	}
	// A split without maps: every branch, no tags.
	if got := apply(nil, "services/api"); len(got) != 3 || got["refs/tags/v0.1"] != "" {
		t.Errorf("split default: %v", got)
	}
	// Tags only: every branch, and the chosen tags (renamed).
	if got := apply([]string{"refs/tags/api-v*:refs/tags/v*"}, "services/api"); len(got) != 4 || got["refs/heads/api"] != "a" || got["refs/tags/v1.0"] != "t1" {
		t.Errorf("tags only: %v", got)
	}
	// Branches only: those branches, no tags.
	if got := apply([]string{"web:main"}, ""); !reflect.DeepEqual(got, map[string]string{"refs/heads/main": "w"}) {
		t.Errorf("branches only: %v", got)
	}
	// One branch renamed, component tags renamed.
	got := apply([]string{"api:main", "refs/tags/api-v*:refs/tags/v*"}, "")
	want := map[string]string{"refs/heads/main": "a", "refs/tags/v1.0": "t1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mapped: %v, want %v", got, want)
	}
	// Two hub refs on one mirror ref: refused.
	v, _ := ParseView([]string{"api:main", "web:main"}, "")
	if _, _, err := v.Apply(hub); err == nil || !strings.Contains(err.Error(), "map to refs/heads/main") {
		t.Errorf("collision: %v", err)
	}
	// The first matching map wins.
	if got := apply([]string{"main:stable", "*:*"}, ""); got["refs/heads/stable"] != "m" || got["refs/heads/main"] != "" || got["refs/heads/api"] != "a" {
		t.Errorf("first match: %v", got)
	}
	// * needs a non-empty match.
	if got := apply([]string{"refs/tags/v*:refs/tags/v*"}, ""); len(got) != 4 || got["refs/tags/v0.1"] != "t0" {
		t.Errorf("tag prefix: %v", got)
	}
}

func TestViewDefaultBranch(t *testing.T) {
	v, _ := ParseView([]string{"api:main"}, "")
	if d := v.DefaultBranch("main", map[string]string{"refs/heads/main": "x"}); d != "main" {
		t.Errorf("single branch: %q", d)
	}
	v, _ = ParseView([]string{"main:trunk", "dev"}, "")
	if d := v.DefaultBranch("main", map[string]string{"refs/heads/trunk": "x", "refs/heads/dev": "y"}); d != "trunk" {
		t.Errorf("renamed default: %q", d)
	}
	v, _ = ParseView([]string{"a", "b"}, "")
	if d := v.DefaultBranch("main", map[string]string{"refs/heads/a": "x", "refs/heads/b": "y"}); d != "" {
		t.Errorf("ambiguous: %q", d)
	}
}

func TestViewDescribe(t *testing.T) {
	for _, c := range []struct {
		maps []string
		sub  string
		want string
	}{
		{nil, "", ""},
		{nil, "services/api", "directory services/api/, every branch, no tags"},
		{[]string{"refs/tags/api-v*:refs/tags/v*"}, "services/api", "directory services/api/, every branch, tag api-v* → tag v*"},
		{[]string{"web:main"}, "", "web → main, no tags"},
		{[]string{"main", "refs/tags/v*"}, "", "main, tag v*"},
	} {
		v, err := ParseView(c.maps, c.sub)
		if err != nil {
			t.Fatal(err)
		}
		if got := v.Describe(); got != c.want {
			t.Errorf("%v %q: %q, want %q", c.maps, c.sub, got, c.want)
		}
	}
}
