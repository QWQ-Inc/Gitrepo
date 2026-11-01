package githooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/qwq-inc/gitrepo/internal/gitexec"
)

// EnvProtected carries the protection rules that apply to the repository
// being pushed to (JSON list of ProtectRule).
const EnvProtected = "GITREPO_PROTECTED"

// ProtectRule keeps matching refs from being force-pushed or deleted.
type ProtectRule struct {
	// Ref is a branch name or pattern ("main", "release/*") or a full ref
	// pattern ("refs/tags/v*"). As in path.Match, * does not cross "/".
	Ref        string `json:"ref"`
	DenyForce  bool   `json:"deny_force"`
	DenyDelete bool   `json:"deny_delete"`
}

// FullRefPattern turns a branch pattern into a ref pattern.
func FullRefPattern(p string) string {
	if strings.HasPrefix(p, "refs/") {
		return p
	}
	return "refs/heads/" + p
}

// ValidateRefPattern checks a rule's ref pattern.
func ValidateRefPattern(p string) error {
	full := FullRefPattern(p)
	if p == "" || strings.ContainsAny(p, " \t\n") {
		return errors.New("a branch name or pattern is required")
	}
	if !strings.HasPrefix(full, "refs/heads/") && !strings.HasPrefix(full, "refs/tags/") {
		return errors.New("only branches and tags (refs/heads/…, refs/tags/…) can be protected")
	}
	if _, err := path.Match(full, "refs/heads/x"); err != nil {
		return fmt.Errorf("invalid pattern %q", p)
	}
	return nil
}

// Matches reports whether the rule covers ref.
func (r ProtectRule) Matches(ref string) bool {
	ok, _ := path.Match(FullRefPattern(r.Ref), ref)
	return ok
}

func parseRules(s string) ([]ProtectRule, error) {
	if s == "" {
		return nil, nil
	}
	var rules []ProtectRule
	err := json.Unmarshal([]byte(s), &rules)
	return rules, err
}

// isAncestor reports whether old is contained in new (a fast-forward).
var isAncestor = defaultIsAncestor

func defaultIsAncestor(old, new string) (bool, error) {
	err := exec.Command(gitBin(), "merge-base", "--is-ancestor", old, new).Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		return false, nil
	default:
		return false, err
	}
}

// checkProtected reports updates the rules forbid: deleting a protected
// ref, rewriting a protected branch (not a fast-forward), or moving a
// protected tag.
func checkProtected(rules []ProtectRule, updates []RefUpdate) []string {
	var problems []string
	for _, u := range updates {
		var force, del bool
		for _, r := range rules {
			if r.Matches(u.Ref) {
				force = force || r.DenyForce
				del = del || r.DenyDelete
			}
		}
		created, deleted := gitexec.IsZero(u.Old), gitexec.IsZero(u.New)
		switch {
		case created || u.Old == u.New:
		case deleted:
			if del {
				problems = append(problems, fmt.Sprintf("%s: protected on the hub; it cannot be deleted", u.Ref))
			}
		case force && strings.HasPrefix(u.Ref, "refs/tags/"):
			problems = append(problems, fmt.Sprintf("%s: protected on the hub; the tag cannot be moved", u.Ref))
		case force:
			ff, err := isAncestor(u.Old, u.New)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: cannot check whether this is a fast-forward: %v", u.Ref, err))
			} else if !ff {
				problems = append(problems, fmt.Sprintf("%s: protected on the hub; force-pushes (rewriting its history) are refused", u.Ref))
			}
		}
	}
	return problems
}

func protectedFromEnv() ([]ProtectRule, error) { return parseRules(os.Getenv(EnvProtected)) }
