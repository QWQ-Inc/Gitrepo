package githooks

import (
	"strings"
	"testing"
)

func TestProtectRules(t *testing.T) {
	const zero = "0000000000000000000000000000000000000000"
	// "ff" pairs are fast-forwards; everything else is a rewrite.
	isAncestor = func(old, new string) (bool, error) { return old+new == "aaaabbbb", nil }
	// Backups: what a deleted ref pointed to.
	backups := map[string]string{"refs/heads/develop": "aaaa", "refs/tags/v0.9": "aaaa"}
	lastBackup = func(ref string) (string, bool, error) { v, ok := backups[ref]; return v, ok, nil }
	defer func() { isAncestor, lastBackup = defaultIsAncestor, defaultLastBackup }()
	a, b, c := "aaaa", "bbbb", "cccc"

	rules := []ProtectRule{
		{Ref: "main", DenyForce: true, DenyDelete: true},
		{Ref: "release/*", DenyForce: true},
		{Ref: "refs/tags/v*", DenyForce: true, DenyDelete: true},
		{Ref: "develop", DenyForce: true},
	}
	cases := []struct {
		u    RefUpdate
		want string // substring of the problem, "" = allowed
	}{
		{RefUpdate{Old: a, New: b, Ref: "refs/heads/main"}, ""},                     // fast-forward
		{RefUpdate{Old: a, New: c, Ref: "refs/heads/main"}, "force-pushes"},         // rewrite
		{RefUpdate{Old: a, New: zero, Ref: "refs/heads/main"}, "cannot be deleted"}, // delete
		{RefUpdate{Old: zero, New: a, Ref: "refs/heads/main"}, ""},                  // create
		{RefUpdate{Old: a, New: c, Ref: "refs/heads/release/1.0"}, "force-pushes"},  // pattern
		{RefUpdate{Old: a, New: zero, Ref: "refs/heads/release/1.0"}, ""},           // deletion allowed by that rule
		{RefUpdate{Old: a, New: c, Ref: "refs/heads/release/1.0/hotfix"}, ""},       // * does not cross /
		{RefUpdate{Old: a, New: c, Ref: "refs/heads/feature"}, ""},                  // unprotected
		{RefUpdate{Old: a, New: c, Ref: "refs/heads/mainline"}, ""},                 // exact name only
		{RefUpdate{Old: a, New: b, Ref: "refs/tags/v1.0"}, "cannot be moved"},       // tags: any move
		{RefUpdate{Old: a, New: zero, Ref: "refs/tags/v1.0"}, "cannot be deleted"},
		{RefUpdate{Old: zero, New: a, Ref: "refs/tags/v2.0"}, ""},
		{RefUpdate{Old: a, New: c, Ref: "refs/tags/nightly"}, ""},
		{RefUpdate{Old: a, New: a, Ref: "refs/heads/main"}, ""},
		// Deleted, then re-created: only with its earlier history.
		{RefUpdate{Old: a, New: zero, Ref: "refs/heads/develop"}, ""},
		{RefUpdate{Old: zero, New: b, Ref: "refs/heads/develop"}, ""},
		{RefUpdate{Old: zero, New: c, Ref: "refs/heads/develop"}, "without its earlier history"},
		{RefUpdate{Old: zero, New: a, Ref: "refs/tags/v0.9"}, ""},
		{RefUpdate{Old: zero, New: b, Ref: "refs/tags/v0.9"}, "cannot be re-created on another commit"},
	}
	for _, c := range cases {
		got := strings.Join(checkProtected(rules, []RefUpdate{c.u}), "; ")
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s %s→%s: %q, want %q", c.u.Ref, c.u.Old, c.u.New, got, c.want)
		}
	}

	for p, ok := range map[string]bool{
		"main": true, "release/*": true, "refs/tags/v*": true, "refs/heads/x": true,
		"": false, "refs/notes/x": false, "refs/backup/*": false, "a b": false, "[": false,
	} {
		if err := ValidateRefPattern(p); (err == nil) != ok {
			t.Errorf("ValidateRefPattern(%q) = %v", p, err)
		}
	}
	if _, err := parseRules("unavailable"); err == nil {
		t.Error("an unreadable rule list must be an error (the push is refused)")
	}
}
