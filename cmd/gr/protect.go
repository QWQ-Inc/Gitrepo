package main

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

type protection struct {
	ID         int64  `json:"id"`
	Repos      string `json:"repos"`
	Ref        string `json:"ref"`
	DenyForce  bool   `json:"deny_force"`
	DenyDelete bool   `json:"deny_delete"`
}

func (p protection) rules() string {
	var r []string
	if p.DenyForce {
		r = append(r, "no force-push")
	}
	if p.DenyDelete {
		r = append(r, "no deletion")
	}
	return strings.Join(r, ", ")
}

func protectCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "protect", Short: "Protect branches and tags on the hub from force-pushes and deletion"}
	var allowForce, allowDelete bool
	add := &cobra.Command{
		Use:   "add REPOS BRANCH",
		Short: "Protect a branch (or pattern) in the matching repositories",
		Long: `Protect a branch in the matching repositories: pushes that rewrite its
history (force-pushes) or delete it are refused on the hub, over HTTPS and
SSH alike.

REPOS is a repository or pattern (acme/app, acme/*, '*'). BRANCH is a
branch name or pattern (main, release/*); for tags use the full name
(refs/tags/v*), which also keeps tags from being moved. As in shell
globs, * does not match across "/".`,
		Example: `  gr protect add '*' main
  gr protect add 'acme/*' 'release/*'
  gr protect add acme/app 'refs/tags/v*'
  gr protect add acme/app develop --allow-delete   # only refuse force-pushes`,
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			var p protection
			if err := call(http.MethodPost, "/api/protections", map[string]any{
				"repos": args[0], "ref": args[1], "deny_force": !allowForce, "deny_delete": !allowDelete,
			}, &p); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(p)
			}
			fmt.Printf("protected %s in %s (%s); rule %d\n", p.Ref, p.Repos, p.rules(), p.ID)
			return nil
		},
	}
	add.Flags().BoolVar(&allowForce, "allow-force", false, "only refuse deletion")
	add.Flags().BoolVar(&allowDelete, "allow-delete", false, "only refuse force-pushes")
	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List protection rules", Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			var ps []protection
			if err := call(http.MethodGet, "/api/protections", nil, &ps); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(ps)
			}
			w := table()
			fmt.Fprintln(w, "ID\tREPOS\tREF\tRULES")
			for _, p := range ps {
				fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", p.ID, p.Repos, p.Ref, p.rules())
			}
			return w.Flush()
		},
	}
	rm := &cobra.Command{
		Use: "rm ID", Short: "Remove a protection rule", Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := call(http.MethodDelete, "/api/protections/"+args[0], nil, nil); err != nil {
				return err
			}
			fmt.Println("removed rule", args[0])
			return nil
		},
	}
	cmd.AddCommand(add, list, rm)
	return cmd
}
