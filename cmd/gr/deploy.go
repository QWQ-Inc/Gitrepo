package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qwq-inc/gitrepo/internal/client"
)

type deployFlags struct {
	rawURL, urlEnv, secretEnv, tokenEnv, branch, source string
	stackenigma, clearSecret, clearToken                bool
}

func (f *deployFlags) register(c *cobra.Command) {
	c.Flags().StringVar(&f.rawURL, "url", "", "the platform's deploy hook URL")
	c.Flags().StringVar(&f.urlEnv, "url-env", "", "read the URL from this environment variable (when it embeds a token)")
	c.Flags().StringVar(&f.secretEnv, "secret-env", "", `signing secret from this environment variable ("whsec_…" for Standard Webhooks)`)
	c.Flags().StringVar(&f.tokenEnv, "token-env", "", "send this environment variable's value as a Bearer token")
	c.Flags().StringVar(&f.branch, "branch", "", "branch to deploy, as named on the source (default: the default branch)")
	c.Flags().StringVar(&f.source, "after", "", `where the platform builds from: "hub" (default) or a mirror, e.g. github:acme/app; the hook fires once the commit is there`)
	c.Flags().BoolVar(&f.stackenigma, "stackenigma", false, "StackEnigma preset")
}

func envValue(name string) (string, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return "", fmt.Errorf("environment variable %s is empty", name)
	}
	return v, nil
}

func (f *deployFlags) body(c *cobra.Command) (map[string]any, error) {
	body := map[string]any{}
	switch {
	case f.urlEnv != "":
		v, err := envValue(f.urlEnv)
		if err != nil {
			return nil, err
		}
		body["url"] = v
	case f.rawURL != "":
		body["url"] = f.rawURL
	}
	for flag, key := range map[string]string{"secret-env": "secret", "token-env": "token"} {
		if name, _ := c.Flags().GetString(flag); name != "" {
			v, err := envValue(name)
			if err != nil {
				return nil, err
			}
			body[key] = v
		}
	}
	if c.Flags().Changed("branch") {
		body["branch"] = f.branch
	}
	if c.Flags().Changed("after") {
		body["source"] = f.source
	}
	if f.stackenigma {
		body["kind"] = "stackenigma"
	}
	if f.clearSecret {
		body["clear_secret"] = true
	}
	if f.clearToken {
		body["clear_token"] = true
	}
	return body, nil
}

// deployHookID finds a hook by id or name.
func deployHookID(p, ref string) (string, error) {
	var hs []client.DeployHook
	if err := call(http.MethodGet, p+"/deploy-hooks", nil, &hs); err != nil {
		return "", err
	}
	for _, h := range hs {
		if h.Name == ref || fmt.Sprint(h.ID) == ref {
			return fmt.Sprint(h.ID), nil
		}
	}
	return "", fmt.Errorf("no deploy hook %q", ref)
}

func deployCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Deploy hooks: tell StackEnigma (or any platform) when a branch has a new commit",
		Long: `Deploy hooks tell a deployment platform that a branch has a new commit.

A hook fires when the branch changes on its source: the hub (right after the
push), or a mirror (--after github:acme/app: once the sync has put the commit
there, for a platform that builds from that mirror).

Each delivery is a POST of JSON {"type":"deploy.requested","data":{...}} with
Standard Webhooks headers (webhook-id, webhook-timestamp and, with a secret,
webhook-signature), X-Gitrepo-Signature-256 with a secret, and
Authorization: Bearer with a token. Failed deliveries are retried; while one
is in flight, only the newest commit waits.`,
	}
	var f deployFlags
	add := &cobra.Command{
		Use:   "add ORG/NAME HOOK",
		Short: "Add a deploy hook to a repository",
		Example: `  # StackEnigma builds from the GitHub mirror: deploy once GitHub has the commit
  gr deploy add acme/app prod --stackenigma --url-env SE_DEPLOY_URL --secret-env SE_DEPLOY_SECRET --after github:acme/app
  # A platform that builds from the hub itself
  gr deploy add acme/app staging --url https://deploy.example.com/hooks/app --token-env DEPLOY_TOKEN --branch develop`,
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			p, err := repoPath(args[0])
			if err != nil {
				return err
			}
			body, err := f.body(c)
			if err != nil {
				return err
			}
			if body["url"] == nil {
				return errors.New("--url or --url-env is required")
			}
			body["name"] = args[1]
			var h client.DeployHook
			if err := call(http.MethodPost, p+"/deploy-hooks", body, &h); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(h)
			}
			fmt.Printf("deploy hook %s added: %s on %s → %s\ntry it: gr deploy trigger %s %s\n", h.Name, h.Branch, h.Source, h.Host, args[0], h.Name)
			return nil
		},
	}
	f.register(add)

	var enable, disable bool
	set := &cobra.Command{
		Use: "set ORG/NAME HOOK", Short: "Change a deploy hook (only the flags given)", Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			p, err := repoPath(args[0])
			if err != nil {
				return err
			}
			body, err := f.body(c)
			if err != nil {
				return err
			}
			if enable && disable {
				return errors.New("--enable and --disable are exclusive")
			}
			if enable || disable {
				body["enabled"] = enable
			}
			if len(body) == 0 {
				return errors.New("nothing to change")
			}
			id, err := deployHookID(p, args[1])
			if err != nil {
				return err
			}
			var h client.DeployHook
			if err := call(http.MethodPatch, p+"/deploy-hooks/"+id, body, &h); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(h)
			}
			fmt.Printf("deploy hook %s updated: %s on %s → %s, enabled %v\n", h.Name, h.Branch, h.Source, h.Host, h.Enabled)
			return nil
		},
	}
	f.register(set)
	set.Flags().BoolVar(&f.clearSecret, "clear-secret", false, "stop signing deliveries")
	set.Flags().BoolVar(&f.clearToken, "clear-token", false, "stop sending the bearer token")
	set.Flags().BoolVar(&enable, "enable", false, "enable the hook")
	set.Flags().BoolVar(&disable, "disable", false, "disable the hook")

	list := &cobra.Command{
		Use: "list ORG/NAME", Aliases: []string{"ls"}, Short: "List a repository's deploy hooks", Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			p, err := repoPath(args[0])
			if err != nil {
				return err
			}
			var hs []client.DeployHook
			if err := call(http.MethodGet, p+"/deploy-hooks", nil, &hs); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(hs)
			}
			w := table()
			fmt.Fprintln(w, "ID\tNAME\tBRANCH\tAFTER\tHOST\tENABLED\tLAST DELIVERY")
			for _, h := range hs {
				last := "never"
				if h.LastAt != nil {
					last = h.LastStatus + " (" + ago(h.LastAt) + ")"
					if h.LastCommit != "" {
						last = fmt.Sprintf("%.10s: %s", h.LastCommit, last)
					}
				}
				fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%v\t%s\n", h.ID, h.Name, h.Branch, h.Source, h.Host, h.Enabled, last)
			}
			return w.Flush()
		},
	}
	trigger := &cobra.Command{
		Use: "trigger ORG/NAME HOOK", Short: "Deploy the branch's current commit now", Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			p, err := repoPath(args[0])
			if err != nil {
				return err
			}
			id, err := deployHookID(p, args[1])
			if err != nil {
				return err
			}
			var r client.DeployTrigger
			if err := call(http.MethodPost, p+"/deploy-hooks/"+id+"/trigger", nil, &r); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(r)
			}
			if !r.OK {
				return fmt.Errorf("deploy of %.10s failed: %s", r.Commit, r.Status)
			}
			fmt.Printf("deploy of %.10s requested: %s\n", r.Commit, r.Status)
			return nil
		},
	}
	rm := &cobra.Command{
		Use: "rm ORG/NAME HOOK", Short: "Remove a deploy hook", Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			p, err := repoPath(args[0])
			if err != nil {
				return err
			}
			id, err := deployHookID(p, args[1])
			if err != nil {
				return err
			}
			if err := call(http.MethodDelete, p+"/deploy-hooks/"+url.PathEscape(id), nil, nil); err != nil {
				return err
			}
			fmt.Printf("deploy hook %s removed\n", args[1])
			return nil
		},
	}
	cmd.AddCommand(add, set, list, trigger, rm)
	return cmd
}
