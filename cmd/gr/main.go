// Command gr is the Gitrepo command-line client.
//
//	export GITREPO_URL=https://git.example.com GITREPO_TOKEN=…
//	gr target add github --provider github --namespace acme --token-env GITHUB_TOKEN
//	gr bind '*' github
//	git push https://git.example.com/acme/demo.git main
//	gr status acme/demo
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/qwq-inc/gitrepo/internal/client"
)

var version = "dev"

var (
	flagURL   string
	flagToken string
	flagJSON  bool
)

func main() {
	root := &cobra.Command{
		Use:           "gr",
		Short:         "Gitrepo client: manage targets, mirrors and sync status",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}
	root.PersistentFlags().StringVar(&flagURL, "url", envOr("GITREPO_URL", "http://localhost:8080"), "gitrepo server URL (env GITREPO_URL)")
	root.PersistentFlags().StringVar(&flagToken, "token", os.Getenv("GITREPO_TOKEN"), "admin token (env GITREPO_TOKEN)")
	root.PersistentFlags().BoolVar(&flagJSON, "json", false, "print raw JSON")

	root.AddCommand(targetCmd(), bindCmd(), bindingCmd(), repoCmd(), remoteCmd(), syncCmd(), statusCmd(), runsCmd(), tokenCmd(), keyCmd(), sshInfoCmd(), webhookCmd(), backupCmd(), auditCmd(), releaseCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func api() *client.Client { return client.New(flagURL, flagToken) }

func call(method, path string, in, out any) error {
	return api().Do(context.Background(), method, path, in, out)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func table() *tabwriter.Writer { return tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0) }

func repoPath(slug string) (string, error) {
	slug = strings.TrimSuffix(strings.Trim(slug, "/"), ".git")
	org, name, ok := strings.Cut(slug, "/")
	if !ok || org == "" || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("repository must be <org>/<name>, got %q", slug)
	}
	return "/api/repos/" + url.PathEscape(org) + "/" + url.PathEscape(name), nil
}

func ago(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	d := time.Since(*t)
	switch {
	case d < 0:
		return "in " + (-d).Round(time.Second).String()
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	if sha == "" {
		return "-"
	}
	return sha
}

// ------------------------------------------------------------------ target

type targetFlags struct {
	provider, baseURL, apiURL, namespace, urlTemplate, visibility string
	noAutoCreate                                                  bool
	maxFileSize, maxLFSSize                                       int64
	username, tokenEnv, sshKeyFile, knownHostsFile, expires       string
	tokenStdin, passwordStdin, noCredential                       bool
}

func (f *targetFlags) register(c *cobra.Command, create bool) {
	fl := c.Flags()
	fl.StringVar(&f.provider, "provider", "", "github | gitlab | gitea (also Forgejo) | gitee | generic")
	fl.StringVar(&f.baseURL, "base-url", "", "web base URL for self-hosted instances (e.g. https://gitlab.example.com)")
	fl.StringVar(&f.apiURL, "api-url", "", "API base URL if it cannot be derived from --base-url")
	fl.StringVar(&f.namespace, "namespace", "", "default owner: GitHub user/org, GitLab user/group path")
	fl.StringVar(&f.urlTemplate, "url-template", "", "generic: URL with {namespace} and {name}, e.g. ssh://git@git.example.com/{namespace}/{name}.git")
	fl.StringVar(&f.visibility, "visibility", "", "force mirror visibility (private|public|internal); default follows the repository")
	fl.BoolVar(&f.noAutoCreate, "no-auto-create", false, "do not create missing repositories on the platform")
	fl.Int64Var(&f.maxFileSize, "max-file-size", 0, "override the platform's per-file size limit in bytes")
	fl.Int64Var(&f.maxLFSSize, "max-lfs-object-size", 0, "override the platform's Git LFS object size limit in bytes (GitHub default: 2 GB)")
	fl.StringVar(&f.username, "username", "", "username for basic/token credentials on generic targets")
	fl.BoolVar(&f.tokenStdin, "token-stdin", false, "read an access token from stdin")
	fl.StringVar(&f.tokenEnv, "token-env", "", "read an access token from this environment variable")
	fl.BoolVar(&f.passwordStdin, "password-stdin", false, "generic: read a password from stdin (basic auth, needs --username)")
	fl.StringVar(&f.sshKeyFile, "ssh-key-file", "", "generic: private key file for SSH remotes")
	fl.StringVar(&f.knownHostsFile, "known-hosts-file", "", "generic: pin SSH host keys with this known_hosts file")
	fl.StringVar(&f.expires, "expires", "", "credential expiry (YYYY-MM-DD) for alerting")
	fl.BoolVar(&f.noCredential, "no-credential", false, "generic: anonymous access")
	if create {
		c.MarkFlagRequired("provider")
	}
}

func (f *targetFlags) body(c *cobra.Command) (map[string]any, error) {
	b := map[string]any{}
	strFlags := map[string]*string{
		"provider": &f.provider, "base-url": &f.baseURL, "api-url": &f.apiURL, "namespace": &f.namespace,
		"url-template": &f.urlTemplate, "visibility": &f.visibility, "expires": &f.expires,
	}
	keys := map[string]string{"provider": "provider", "base-url": "base_url", "api-url": "api_url", "namespace": "namespace",
		"url-template": "url_template", "visibility": "visibility", "expires": "expires_at"}
	for flag, p := range strFlags {
		if c.Flags().Changed(flag) {
			b[keys[flag]] = *p
		}
	}
	if c.Flags().Changed("no-auto-create") {
		b["auto_create"] = !f.noAutoCreate
	}
	if c.Flags().Changed("max-file-size") {
		b["max_file_size"] = f.maxFileSize
	}
	if c.Flags().Changed("max-lfs-object-size") {
		b["max_lfs_object_size"] = f.maxLFSSize
	}

	var cred map[string]any
	readStdin := func() (string, error) {
		raw, err := io.ReadAll(os.Stdin)
		return strings.TrimSpace(string(raw)), err
	}
	switch {
	case f.noCredential:
		cred = map[string]any{"kind": "none"}
	case f.tokenStdin || f.tokenEnv != "":
		var tok string
		var err error
		if f.tokenStdin {
			tok, err = readStdin()
		} else {
			tok = strings.TrimSpace(os.Getenv(f.tokenEnv))
			if tok == "" {
				err = fmt.Errorf("environment variable %s is empty", f.tokenEnv)
			}
		}
		if err != nil {
			return nil, err
		}
		cred = map[string]any{"kind": "token", "secret": tok, "username": f.username}
	case f.passwordStdin:
		if f.username == "" {
			return nil, errors.New("--password-stdin needs --username")
		}
		pw, err := readStdin()
		if err != nil {
			return nil, err
		}
		cred = map[string]any{"kind": "basic", "username": f.username, "secret": pw}
	case f.sshKeyFile != "":
		key, err := os.ReadFile(f.sshKeyFile)
		if err != nil {
			return nil, err
		}
		cred = map[string]any{"kind": "ssh", "secret": string(key)}
		if f.knownHostsFile != "" {
			kh, err := os.ReadFile(f.knownHostsFile)
			if err != nil {
				return nil, err
			}
			cred["ssh_known_hosts"] = string(kh)
		}
	}
	if cred != nil {
		b["credential"] = cred
	}
	return b, nil
}

func targetCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "target", Short: "Platform accounts (provider + credential + namespace)"}

	var af targetFlags
	add := &cobra.Command{
		Use:   "add NAME",
		Short: "Add a target",
		Example: `  gr target add github --provider github --namespace acme --token-env GITHUB_TOKEN
  gr target add gitlab --provider gitlab --namespace acme-group --token-stdin < token.txt
  gr target add internal --provider generic --url-template 'ssh://git@git.example.com/{namespace}/{name}.git' --ssh-key-file ~/.ssh/mirror`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			b, err := af.body(c)
			if err != nil {
				return err
			}
			b["name"] = args[0]
			if _, ok := b["credential"]; !ok && af.provider != "generic" {
				return errors.New("a credential is required: use --token-env VAR or --token-stdin")
			}
			if _, ok := b["credential"]; !ok {
				b["credential"] = map[string]any{"kind": "none"}
			}
			var t client.Target
			if err := call(http.MethodPost, "/api/targets", b, &t); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(t)
			}
			fmt.Printf("target %s added (%s, namespace %q)\n", t.Name, t.Provider, t.Namespace)
			fmt.Printf("next: gr target check %s   and   gr bind '*' %s\n", t.Name, t.Name)
			return nil
		},
	}
	af.register(add, true)

	var uf targetFlags
	update := &cobra.Command{
		Use:   "update NAME",
		Short: "Change a target (only the given flags; rotate the credential with --token-env/--token-stdin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			b, err := uf.body(c)
			if err != nil {
				return err
			}
			var t client.Target
			if err := call(http.MethodPatch, "/api/targets/"+url.PathEscape(args[0]), b, &t); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(t)
			}
			fmt.Printf("target %s updated\n", t.Name)
			return nil
		},
	}
	uf.register(update, false)

	list := &cobra.Command{
		Use: "list", Short: "List targets", Aliases: []string{"ls"}, Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			var ts []client.Target
			if err := call(http.MethodGet, "/api/targets", nil, &ts); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(ts)
			}
			w := table()
			fmt.Fprintln(w, "NAME\tPROVIDER\tNAMESPACE\tURL\tCREDENTIAL\tAUTO-CREATE\tEXPIRES")
			for _, t := range ts {
				u := t.BaseURL
				if t.URLTemplate != "" {
					u = t.URLTemplate
				}
				if u == "" {
					u = "-"
				}
				exp := "-"
				if t.ExpiresAt != nil {
					exp = t.ExpiresAt.Format("2006-01-02")
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%v\t%s\n", t.Name, t.Provider, t.Namespace, u, t.CredKind, t.AutoCreate, exp)
			}
			return w.Flush()
		},
	}
	rm := &cobra.Command{
		Use: "rm NAME", Short: "Remove a target (fails while remotes use it)", Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := call(http.MethodDelete, "/api/targets/"+url.PathEscape(args[0]), nil, nil); err != nil {
				return err
			}
			fmt.Println("removed target", args[0])
			return nil
		},
	}
	check := &cobra.Command{
		Use: "check NAME", Short: "Verify a target's credential against the platform API", Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			var res struct {
				OK       bool   `json:"ok"`
				Identity string `json:"identity"`
				Error    string `json:"error"`
			}
			if err := call(http.MethodPost, "/api/targets/"+url.PathEscape(args[0])+"/check", nil, &res); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(res)
			}
			if !res.OK {
				return fmt.Errorf("check failed: %s", res.Error)
			}
			fmt.Printf("ok: authenticated as %s\n", res.Identity)
			return nil
		},
	}
	cmd.AddCommand(add, update, list, rm, check)
	return cmd
}

// ----------------------------------------------------------------- binding

type bindFlags struct {
	namespace, onDivergence, nameTemplate string
	applyExisting                         bool
}

func addBinding(f *bindFlags, org, target string) error {
	var res struct {
		Binding client.Binding `json:"binding"`
		Applied int            `json:"applied_to_repos"`
		Skipped []string       `json:"skipped"`
	}
	err := call(http.MethodPost, "/api/bindings", map[string]any{
		"org": org, "target": target, "namespace": f.namespace, "on_divergence": f.onDivergence,
		"name_template": f.nameTemplate, "apply_existing": f.applyExisting,
	}, &res)
	if err != nil {
		return err
	}
	if flagJSON {
		return printJSON(res)
	}
	scope := "org " + org
	if org == "*" {
		scope = "every org"
	}
	fmt.Printf("new repositories in %s will mirror to %s (binding %d)\n", scope, target, res.Binding.ID)
	if f.applyExisting {
		fmt.Printf("added to %d existing repositories\n", res.Applied)
		for _, s := range res.Skipped {
			fmt.Println("  skipped", s)
		}
	}
	return nil
}

func (f *bindFlags) register(c *cobra.Command) {
	c.Flags().StringVar(&f.namespace, "namespace", "", "namespace on the target (default: the target's namespace)")
	c.Flags().StringVar(&f.onDivergence, "on-divergence", "alert", "alert | overwrite")
	c.Flags().BoolVar(&f.applyExisting, "apply-existing", false, "also add the mirror to existing repositories of the org")
	c.Flags().StringVar(&f.nameTemplate, "name-template", "{name}", "mirror repository name; {org} and {name} are the hub repository's, e.g. '{org}-{name}' when several orgs share one namespace")
}

func bindCmd() *cobra.Command {
	var f bindFlags
	c := &cobra.Command{
		Use:     "bind ORG|'*' TARGET",
		Short:   "Mirror every new repository of an org (or all orgs) to a target",
		Example: "  gr bind '*' github\n  gr bind acme gitlab --namespace acme-mirrors",
		Args:    cobra.ExactArgs(2),
		RunE:    func(c *cobra.Command, args []string) error { return addBinding(&f, args[0], args[1]) },
	}
	f.register(c)
	return c
}

func bindingCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "binding", Short: "Org → target defaults applied to new repositories"}
	var f bindFlags
	add := &cobra.Command{
		Use: "add ORG|'*' TARGET", Short: "Same as gr bind", Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error { return addBinding(&f, args[0], args[1]) },
	}
	f.register(add)
	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List bindings", Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			var bs []client.Binding
			if err := call(http.MethodGet, "/api/bindings", nil, &bs); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(bs)
			}
			w := table()
			fmt.Fprintln(w, "ID\tORG\tTARGET\tNAMESPACE\tMIRROR NAME\tON-DIVERGENCE")
			for _, b := range bs {
				ns := b.Namespace
				if ns == "" {
					ns = "(target default)"
				}
				fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", b.ID, b.Org, b.Target, ns, b.NameTemplate, b.OnDivergence)
			}
			return w.Flush()
		},
	}
	rm := &cobra.Command{
		Use: "rm ID", Short: "Remove a binding (existing remotes stay)", Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := call(http.MethodDelete, "/api/bindings/"+args[0], nil, nil); err != nil {
				return err
			}
			fmt.Println("removed binding", args[0])
			return nil
		},
	}
	cmd.AddCommand(add, list, rm)
	return cmd
}

// -------------------------------------------------------------------- repo

func repoCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "repo", Short: "Hub repositories"}
	var public, noBindings bool
	var targets []string
	add := &cobra.Command{
		Use:   "add ORG/NAME",
		Short: "Register a repository (optional: pushing to a new URL creates it too)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			vis := "private"
			if public {
				vis = "public"
			}
			var r client.Repo
			if err := call(http.MethodPost, "/api/repos", map[string]any{
				"slug": args[0], "visibility": vis, "bindings": !noBindings, "targets": targets,
			}, &r); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(r)
			}
			fmt.Printf("created %s\n  push to: %s\n", r.Slug, r.CloneURL)
			for _, rem := range r.Remotes {
				fmt.Printf("  mirror:  %s  %s\n", rem.Label, rem.URL)
			}
			if len(r.Remotes) == 0 {
				fmt.Println("  no mirrors yet: gr remote add", r.Slug, "<target>")
			}
			return nil
		},
	}
	add.Flags().BoolVar(&public, "public", false, "allow anonymous reads and create public mirrors")
	add.Flags().BoolVar(&noBindings, "no-bindings", false, "do not apply org bindings")
	add.Flags().StringSliceVar(&targets, "target", nil, "additional target(s) to mirror to")

	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List repositories", Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error { return statusAll() },
	}
	rm := &cobra.Command{
		Use: "rm ORG/NAME", Short: "Unregister a repository (data is moved to the trash directory, mirrors are untouched)", Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			p, err := repoPath(args[0])
			if err != nil {
				return err
			}
			var res map[string]string
			if err := call(http.MethodDelete, p, nil, &res); err != nil {
				return err
			}
			fmt.Printf("removed %s (data moved to %s)\n", args[0], res["moved_to"])
			return nil
		},
	}
	var from, impNS, impName, impURL string
	var force, impPublic bool
	imp := &cobra.Command{
		Use:   "import ORG/NAME --from TARGET",
		Short: "Make the hub a copy of an existing repository on a platform (onboarding or disaster recovery)",
		Long: `Make the hub a copy of an existing repository on a platform.

Fetches every branch and tag (and Git LFS objects) from the repository the
target holds, makes it the hub's content, records that mirror as adopted
and in sync, and then syncs the other mirrors. Use it to move an existing
project onto Gitrepo, or to rebuild a lost hub from any mirror.

The hub repository is created when missing. If it already has branches or
tags the import is refused unless --force is given; replaced refs are kept
under refs/backup/ on the hub.`,
		Example: "  gr repo import acme/demo --from github\n  gr repo import acme/demo --from gitlab --namespace acme-group --name demo-app",
		Args:    cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			p, err := repoPath(args[0])
			if err != nil {
				return err
			}
			vis := ""
			if impPublic {
				vis = "public"
			}
			var res struct {
				Import struct {
					Refs          int      `json:"refs"`
					LFSObjects    int      `json:"lfs_objects"`
					DefaultBranch string   `json:"default_branch"`
					BackedUp      int      `json:"backed_up"`
					Warnings      []string `json:"warnings"`
				} `json:"import"`
				Source string      `json:"source"`
				Repo   client.Repo `json:"repo"`
			}
			if err := call(http.MethodPost, p+"/import", map[string]any{
				"target": from, "namespace": impNS, "name": impName, "url": impURL, "force": force, "visibility": vis,
			}, &res); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(res)
			}
			fmt.Printf("imported %s from %s: %d ref(s), %d LFS object(s), default branch %s\n",
				res.Repo.Slug, res.Source, res.Import.Refs, res.Import.LFSObjects, res.Import.DefaultBranch)
			if res.Import.BackedUp > 0 {
				fmt.Printf("previous hub refs kept under refs/backup/ (%d)\n", res.Import.BackedUp)
			}
			for _, w := range res.Import.Warnings {
				fmt.Println("warning:", w)
			}
			fmt.Printf("push to: %s\nother mirrors are syncing now: gr status %s\n", res.Repo.CloneURL, res.Repo.Slug)
			return nil
		},
	}
	imp.Flags().StringVar(&from, "from", "", "target that holds the existing repository (required)")
	imp.Flags().StringVar(&impNS, "namespace", "", "namespace on the target (default: the target's)")
	imp.Flags().StringVar(&impName, "name", "", "repository name on the target (default: same as on the hub)")
	imp.Flags().StringVar(&impURL, "url", "", "explicit Git URL of the existing repository")
	imp.Flags().BoolVar(&force, "force", false, "replace existing hub refs (they are backed up)")
	imp.Flags().BoolVar(&impPublic, "public", false, "create the hub repository as public")
	imp.MarkFlagRequired("from")
	cmd.AddCommand(add, list, rm, imp)
	return cmd
}

// ------------------------------------------------------------------ remote

func findRemote(slug, ref string) (*client.Repo, *client.Remote, error) {
	p, err := repoPath(slug)
	if err != nil {
		return nil, nil, err
	}
	var r client.Repo
	if err := call(http.MethodGet, p, nil, &r); err != nil {
		return nil, nil, err
	}
	id, idErr := strconv.ParseInt(ref, 10, 64)
	var match []*client.Remote
	for i := range r.Remotes {
		rem := &r.Remotes[i]
		if (idErr == nil && rem.ID == id) || rem.Label == ref || rem.Target == ref {
			match = append(match, rem)
		}
	}
	switch len(match) {
	case 0:
		return nil, nil, fmt.Errorf("no remote %q on %s (see gr remote list %s)", ref, slug, slug)
	case 1:
		return &r, match[0], nil
	default:
		return nil, nil, fmt.Errorf("%q is ambiguous; use the remote id", ref)
	}
}

func remoteCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "remote", Short: "Mirrors of a repository"}
	var namespace, name, rawURL, onDiv string
	var adopt bool
	add := &cobra.Command{
		Use:     "add ORG/NAME TARGET",
		Short:   "Mirror a repository to a target",
		Example: "  gr remote add acme/demo gitlab\n  gr remote add acme/demo internal --url ssh://git@git.example.com/backup/demo.git",
		Args:    cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			p, err := repoPath(args[0])
			if err != nil {
				return err
			}
			var r client.Repo
			if err := call(http.MethodPost, p+"/remotes", map[string]any{
				"target": args[1], "namespace": namespace, "name": name, "url": rawURL, "on_divergence": onDiv, "adopt": adopt,
			}, &r); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(r)
			}
			fmt.Printf("mirror added; first sync queued (gr status %s)\n", r.Slug)
			return nil
		},
	}
	add.Flags().StringVar(&namespace, "namespace", "", "namespace on the target (default: the target's)")
	add.Flags().StringVar(&name, "name", "", "repository name on the target (default: same as on the hub)")
	add.Flags().StringVar(&rawURL, "url", "", "explicit Git URL (overrides target URL derivation)")
	add.Flags().StringVar(&onDiv, "on-divergence", "alert", "alert | overwrite")
	add.Flags().BoolVar(&adopt, "adopt", false, "the mirror repository already has content: let Gitrepo manage it (existing branches are fast-forwarded or reported, never overwritten)")

	list := &cobra.Command{
		Use: "list ORG/NAME", Aliases: []string{"ls"}, Short: "List mirrors", Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error { return statusOne(args[0]) },
	}
	patch := func(use, short string, body func() map[string]any, done string) *cobra.Command {
		return &cobra.Command{
			Use: use + " ORG/NAME REMOTE", Short: short, Args: cobra.ExactArgs(2),
			RunE: func(c *cobra.Command, args []string) error {
				repo, rem, err := findRemote(args[0], args[1])
				if err != nil {
					return err
				}
				p, _ := repoPath(repo.Slug)
				if err := call(http.MethodPatch, fmt.Sprintf("%s/remotes/%d", p, rem.ID), body(), nil); err != nil {
					return err
				}
				fmt.Printf("%s: %s\n", rem.Label, done)
				return nil
			},
		}
	}
	adoptCmd := patch("adopt", "Let Gitrepo manage a mirror repository that already had content", func() map[string]any { return map[string]any{"adopt": true} }, "adopted; syncing now (foreign refs are fast-forwarded or reported as divergent, never overwritten)")
	pause := patch("pause", "Stop pushing to a mirror (it is still checked)", func() map[string]any { return map[string]any{"enabled": false} }, "paused")
	resume := patch("resume", "Resume pushing to a mirror", func() map[string]any { return map[string]any{"enabled": true} }, "resumed")
	var setDiv string
	set := patch("set", "Change a mirror's divergence policy", func() map[string]any { return map[string]any{"on_divergence": setDiv} }, "updated")
	set.Flags().StringVar(&setDiv, "on-divergence", "alert", "alert | overwrite")
	rm := &cobra.Command{
		Use: "rm ORG/NAME REMOTE", Short: "Stop mirroring to a remote (the remote repository is not deleted)", Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			repo, rem, err := findRemote(args[0], args[1])
			if err != nil {
				return err
			}
			p, _ := repoPath(repo.Slug)
			if err := call(http.MethodDelete, fmt.Sprintf("%s/remotes/%d", p, rem.ID), nil, nil); err != nil {
				return err
			}
			fmt.Printf("removed mirror %s\n", rem.Label)
			return nil
		},
	}
	cmd.AddCommand(add, list, adoptCmd, pause, resume, set, rm)
	return cmd
}

// -------------------------------------------------------------------- sync

func syncCmd() *cobra.Command {
	var overwrite, noWait bool
	var only []string
	c := &cobra.Command{
		Use:   "sync ORG/NAME",
		Short: "Sync a repository to its mirrors now",
		Long: `Sync a repository to its mirrors now.

--overwrite replaces divergent refs on the mirrors with the hub's version for
this run. The mirrors' previous values are kept on the hub under
refs/backup/<time>/remotes/<id>/… first.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			p, err := repoPath(args[0])
			if err != nil {
				return err
			}
			body := map[string]any{"overwrite": overwrite, "wait": !noWait}
			if len(only) > 0 {
				var ids []int64
				for _, ref := range only {
					_, rem, err := findRemote(args[0], ref)
					if err != nil {
						return err
					}
					ids = append(ids, rem.ID)
				}
				body["remote_ids"] = ids
			}
			var res client.SyncResult
			if err := call(http.MethodPost, p+"/sync", body, &res); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(res)
			}
			if noWait {
				fmt.Println("sync queued")
				return nil
			}
			failed := false
			for _, e := range res.Events {
				switch e.Type {
				case "remote":
					line := fmt.Sprintf("%-10s %s", e.State, e.Remote)
					switch {
					case e.Error != "":
						line += "  " + e.Error
						failed = true
					case len(e.Divergent) > 0:
						line += "  " + strings.Join(e.Divergent, ", ")
						failed = true
					case e.Changed > 0:
						line += fmt.Sprintf("  %d ref(s) updated", e.Changed)
					}
					fmt.Println(line)
					for _, w := range e.Warnings {
						fmt.Println("           warning:", w)
					}
				case "done":
					if e.Error != "" {
						return errors.New(e.Error)
					}
				}
			}
			if res.Timeout {
				fmt.Println("still running; check gr status", args[0])
			}
			if failed {
				return errors.New("not all mirrors are in sync")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&overwrite, "overwrite", false, "replace divergent mirror refs (backed up first)")
	c.Flags().BoolVar(&noWait, "no-wait", false, "queue and return immediately")
	c.Flags().StringSliceVar(&only, "remote", nil, "only these remotes (id or target name)")
	return c
}

// ------------------------------------------------------------------ status

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status [ORG/NAME]",
		Short: "Show sync state of all repositories or one repository",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) == 1 {
				return statusOne(args[0])
			}
			return statusAll()
		},
	}
}

func statusAll() error {
	var repos []client.Repo
	if err := call(http.MethodGet, "/api/status", nil, &repos); err != nil {
		return err
	}
	if flagJSON {
		return printJSON(repos)
	}
	if len(repos) == 0 {
		fmt.Println("no repositories yet — push to", strings.TrimRight(flagURL, "/")+"/<org>/<name>.git", "to create one")
		return nil
	}
	w := table()
	fmt.Fprintln(w, "REPO\tSTATE\tMIRRORS\tLAST FULL SYNC\tPUSH URL")
	for _, r := range repos {
		synced := 0
		var oldest *time.Time
		for i, rem := range r.Remotes {
			if rem.State == "synced" {
				synced++
			}
			if i == 0 || rem.LastSuccessAt == nil || (oldest != nil && rem.LastSuccessAt.Before(*oldest)) {
				oldest = rem.LastSuccessAt
			}
		}
		state := r.State
		if r.Busy {
			state += "*"
		}
		fmt.Fprintf(w, "%s\t%s\t%d/%d\t%s\t%s\n", r.Slug, state, synced, len(r.Remotes), ago(oldest), r.CloneURL)
	}
	return w.Flush()
}

func statusOne(slug string) error {
	p, err := repoPath(slug)
	if err != nil {
		return err
	}
	var r client.Repo
	if err := call(http.MethodGet, p, nil, &r); err != nil {
		return err
	}
	if flagJSON {
		return printJSON(r)
	}
	branch := r.DefaultBranch
	if branch == "" {
		branch = "(empty)"
	}
	busy := ""
	if r.Busy {
		busy = " (sync in progress)"
	}
	fmt.Printf("%s  %s, default branch %s\n", r.Slug, r.Visibility, branch)
	fmt.Printf("push/clone: %s\n", r.CloneURL)
	if r.SSHURL != "" {
		fmt.Printf("ssh:        %s\n", r.SSHURL)
	}
	fmt.Printf("state:      %s%s\n\n", r.State, busy)
	if len(r.Remotes) == 0 {
		fmt.Println("no mirrors — gr remote add", r.Slug, "<target>")
		return nil
	}
	w := table()
	fmt.Fprintln(w, "ID\tMIRROR\tSTATE\tLAST SUCCESS\tON-DIVERGENCE\tURL")
	for _, rem := range r.Remotes {
		state := rem.State
		if !rem.Enabled {
			state = "paused"
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", rem.ID, rem.Label, state, ago(rem.LastSuccessAt), rem.OnDivergence, rem.URL)
	}
	w.Flush()
	for _, rem := range r.Remotes {
		if rem.LastError != "" {
			fmt.Printf("\n%s error (%d consecutive failure(s), next retry %s):\n  %s\n", rem.Label, rem.ConsecutiveFailures, ago(rem.NextRetryAt), rem.LastError)
		}
		if rem.NeedsAdoption {
			fmt.Printf("  if %s is the right repository: gr remote adopt %s %d\n", rem.URL, r.Slug, rem.ID)
		}
		if len(rem.Divergences) > 0 {
			fmt.Printf("\n%s diverged:\n", rem.Label)
			for _, d := range rem.Divergences {
				fmt.Printf("  %s  %s (mirror %s, hub %s, last pushed %s)\n", d.Ref, d.Reason, short(d.RemoteSHA), short(d.HubSHA), short(d.ExpectedSHA))
			}
			fmt.Printf("  resolve: merge the mirror's commits and push to the hub, or: gr sync %s --overwrite --remote %d\n", r.Slug, rem.ID)
		}
	}
	return nil
}

func runsCmd() *cobra.Command {
	var limit int
	c := &cobra.Command{
		Use: "runs ORG/NAME", Short: "Show recent sync runs", Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			p, err := repoPath(args[0])
			if err != nil {
				return err
			}
			var runs []client.Run
			if err := call(http.MethodGet, fmt.Sprintf("%s/runs?limit=%d", p, limit), nil, &runs); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(runs)
			}
			w := table()
			fmt.Fprintln(w, "ID\tSTARTED\tMIRROR\tTRIGGER\tSTATUS\tCHANGED\tDURATION\tERROR")
			for _, r := range runs {
				dur := "-"
				if r.FinishedAt != nil && r.StartedAt != nil {
					dur = r.FinishedAt.Sub(*r.StartedAt).Round(10 * time.Millisecond).String()
				}
				fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", r.ID, ago(r.StartedAt), r.Remote, r.Trigger, r.Status, r.RefsChanged, dur, oneLine(r.Error))
			}
			return w.Flush()
		},
	}
	c.Flags().IntVar(&limit, "limit", 20, "number of runs")
	return c
}

// oneLine makes server-provided text (commit subjects, audit details) safe
// for a table row: newlines become spaces, other control and formatting
// characters (terminal escapes, bidi overrides) are shown escaped.
func oneLine(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteRune(r)
		}
	}
	s = b.String()
	if r := []rune(s); len(r) > 120 {
		s = string(r[:120]) + "…"
	}
	return s
}

// ------------------------------------------------------------------- token

func tokenCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Client tokens for git push/clone and the API"}
	var scopes, repos string
	create := &cobra.Command{
		Use:     "create NAME",
		Short:   "Create a token (shown once)",
		Example: "  gr token create laptop --scopes create\n  gr token create ci --scopes read --repos 'acme/*'",
		Args:    cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			var t client.Token
			if err := call(http.MethodPost, "/api/tokens", map[string]any{
				"name": args[0], "scopes": strings.Split(scopes, ","), "repos": splitNonEmpty(repos),
			}, &t); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(t)
			}
			fmt.Println(t.Token)
			fmt.Fprintf(os.Stderr, "token %q created with scopes %s; it will not be shown again.\n", t.Name, strings.Join(t.Scopes, ","))
			fmt.Fprintln(os.Stderr, "use it as the password for git over HTTPS (any username).")
			return nil
		},
	}
	create.Flags().StringVar(&scopes, "scopes", "write", "read | write | create | admin (each implies the previous)")
	create.Flags().StringVar(&repos, "repos", "", "comma-separated repository patterns, e.g. 'acme/*' (default: all)")
	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List tokens", Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			var ts []client.Token
			if err := call(http.MethodGet, "/api/tokens", nil, &ts); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(ts)
			}
			w := table()
			fmt.Fprintln(w, "ID\tNAME\tSCOPES\tREPOS\tLAST USED")
			for _, t := range ts {
				r := strings.Join(t.Repos, ",")
				if r == "" {
					r = "*"
				}
				fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", t.ID, t.Name, strings.Join(t.Scopes, ","), r, ago(t.LastUsedAt))
			}
			return w.Flush()
		},
	}
	rm := &cobra.Command{
		Use: "rm ID", Short: "Revoke a token", Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := call(http.MethodDelete, "/api/tokens/"+args[0], nil, nil); err != nil {
				return err
			}
			fmt.Println("revoked token", args[0])
			return nil
		},
	}
	cmd.AddCommand(create, list, rm)
	return cmd
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ----------------------------------------------------------------- webhook

func webhookCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "webhook", Short: "Outbound notifications (JSON with HMAC, Slack, 企业微信, 飞书, 钉钉)"}
	var rawURL, urlEnv, format, events, repos, secretEnv string
	add := &cobra.Command{
		Use:   "add NAME",
		Short: "Add a webhook",
		Long: `Add a webhook.

Events: push, sync.completed, remote.diverged, remote.failed, remote.recovered,
credential.expiring; "remote.*" or "*" subscribe to groups.

JSON deliveries carry X-Gitrepo-Event, X-Gitrepo-Delivery and, with a secret,
X-Gitrepo-Signature-256: sha256=HMAC-SHA256(secret, body).

Chat-bot URLs contain their access key; pass them with --url-env to keep them
out of shell history. They are stored encrypted.`,
		Example: `  gr webhook add ci --url https://ci.example.com/hooks/gitrepo --events sync.completed --secret-env HOOK_SECRET
  gr webhook add oncall --format wecom --url-env WECOM_BOT_URL --events 'remote.*,credential.expiring'
  gr webhook add team --format slack --url-env SLACK_WEBHOOK_URL --repos 'acme/*'`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			u := rawURL
			if urlEnv != "" {
				u = strings.TrimSpace(os.Getenv(urlEnv))
			}
			if u == "" {
				return errors.New("--url or --url-env is required")
			}
			body := map[string]any{"name": args[0], "url": u, "format": format, "events": splitNonEmpty(events), "repos": splitNonEmpty(repos)}
			if secretEnv != "" {
				sec := strings.TrimSpace(os.Getenv(secretEnv))
				if sec == "" {
					return fmt.Errorf("environment variable %s is empty", secretEnv)
				}
				body["secret"] = sec
			}
			var h client.Webhook
			if err := call(http.MethodPost, "/api/webhooks", body, &h); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(h)
			}
			fmt.Printf("webhook %s added (id %d, %s → %s); try it: gr webhook test %d\n", h.Name, h.ID, h.Format, h.Host, h.ID)
			return nil
		},
	}
	add.Flags().StringVar(&rawURL, "url", "", "endpoint URL")
	add.Flags().StringVar(&urlEnv, "url-env", "", "read the endpoint URL from this environment variable")
	add.Flags().StringVar(&format, "format", "json", "json | slack | wecom | feishu | dingtalk")
	add.Flags().StringVar(&events, "events", "*", "comma-separated events")
	add.Flags().StringVar(&repos, "repos", "", "only events of these repositories, e.g. 'acme/*' (default: all)")
	add.Flags().StringVar(&secretEnv, "secret-env", "", "HMAC signing secret from this environment variable")

	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List webhooks", Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			var hs []client.Webhook
			if err := call(http.MethodGet, "/api/webhooks", nil, &hs); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(hs)
			}
			w := table()
			fmt.Fprintln(w, "ID\tNAME\tFORMAT\tHOST\tEVENTS\tREPOS\tENABLED\tLAST DELIVERY")
			for _, h := range hs {
				r := strings.Join(h.Repos, ",")
				if r == "" {
					r = "*"
				}
				last := "never"
				if h.LastDeliveryAt != nil {
					last = h.LastStatus + " (" + ago(h.LastDeliveryAt) + ")"
				}
				fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%v\t%s\n", h.ID, h.Name, h.Format, h.Host, strings.Join(h.Events, ","), r, h.Enabled, last)
			}
			return w.Flush()
		},
	}
	simple := func(use, short, method, suffix string, body any, done string) *cobra.Command {
		return &cobra.Command{
			Use: use + " ID", Short: short, Args: cobra.ExactArgs(1),
			RunE: func(c *cobra.Command, args []string) error {
				if err := call(method, "/api/webhooks/"+args[0]+suffix, body, nil); err != nil {
					return err
				}
				fmt.Printf("webhook %s %s\n", args[0], done)
				return nil
			},
		}
	}
	test := &cobra.Command{
		Use: "test ID", Short: "Send a test delivery now", Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			var res struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			if err := call(http.MethodPost, "/api/webhooks/"+args[0]+"/test", nil, &res); err != nil {
				return err
			}
			if !res.OK {
				return fmt.Errorf("delivery failed: %s", res.Error)
			}
			fmt.Println("delivered")
			return nil
		},
	}
	cmd.AddCommand(add, list, test,
		simple("disable", "Stop deliveries", http.MethodPatch, "", map[string]any{"enabled": false}, "disabled"),
		simple("enable", "Resume deliveries", http.MethodPatch, "", map[string]any{"enabled": true}, "enabled"),
		simple("rm", "Remove a webhook", http.MethodDelete, "", nil, "removed"))
	return cmd
}

// --------------------------------------------------------------------- ssh

func keyCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "key", Short: "SSH public keys for git over SSH (scoped like tokens)"}
	var scopes, repos, file string
	add := &cobra.Command{
		Use:     "add NAME",
		Short:   "Register an SSH public key",
		Example: "  gr key add laptop --scopes create --file ~/.ssh/id_ed25519.pub\n  gr key add deploy --scopes read --repos 'acme/*' < deploy_key.pub",
		Args:    cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			var raw []byte
			var err error
			if file != "" {
				raw, err = os.ReadFile(file)
			} else {
				raw, err = io.ReadAll(os.Stdin)
			}
			if err != nil {
				return err
			}
			var k client.SSHKey
			if err := call(http.MethodPost, "/api/ssh-keys", map[string]any{
				"name": args[0], "public_key": string(raw), "scopes": strings.Split(scopes, ","), "repos": splitNonEmpty(repos),
			}, &k); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(k)
			}
			fmt.Printf("key %s added (%s %s, scopes %s)\n", k.Name, k.Type, k.Fingerprint, strings.Join(k.Scopes, ","))
			return nil
		},
	}
	add.Flags().StringVar(&scopes, "scopes", "write", "read | write | create | admin")
	add.Flags().StringVar(&repos, "repos", "", "comma-separated repository patterns (default: all)")
	add.Flags().StringVar(&file, "file", "", "public key file (default: read stdin)")
	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List SSH keys", Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			var ks []client.SSHKey
			if err := call(http.MethodGet, "/api/ssh-keys", nil, &ks); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(ks)
			}
			w := table()
			fmt.Fprintln(w, "ID\tNAME\tTYPE\tFINGERPRINT\tSCOPES\tREPOS\tLAST USED")
			for _, k := range ks {
				r := strings.Join(k.Repos, ",")
				if r == "" {
					r = "*"
				}
				fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", k.ID, k.Name, k.Type, k.Fingerprint, strings.Join(k.Scopes, ","), r, ago(k.LastUsedAt))
			}
			return w.Flush()
		},
	}
	rm := &cobra.Command{
		Use: "rm ID", Short: "Remove an SSH key", Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := call(http.MethodDelete, "/api/ssh-keys/"+args[0], nil, nil); err != nil {
				return err
			}
			fmt.Println("removed key", args[0])
			return nil
		},
	}
	cmd.AddCommand(add, list, rm)
	return cmd
}

func sshInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use: "ssh", Short: "Show the SSH endpoint and host key (for known_hosts)", Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			var info client.SSHInfo
			if err := call(http.MethodGet, "/api/ssh", nil, &info); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(info)
			}
			if !info.Enabled {
				fmt.Println("SSH is disabled; set ssh.listen (or GITREPO_SSH_LISTEN) on the server")
				return nil
			}
			host := info.Host
			if h, p, ok := strings.Cut(host, ":"); ok {
				host = "[" + h + "]:" + p
			}
			fmt.Printf("url:         %s\nfingerprint: %s\nknown_hosts: %s %s\n", info.ExampleURL, info.Fingerprint, host, info.HostKey)
			return nil
		},
	}
}

// ------------------------------------------------------------------ backup

func backupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Hidden backups of overwritten history (list, restore, purge)",
		Long: `Hidden backups of overwritten history.

Whenever history is overwritten — a force push or branch deletion on the
hub, a forced import, or a mirror's own commits replaced by --overwrite —
the old commits are kept under refs/backup/ for sync.backup_retention
(default 30 days). They are invisible to clones but still in the hub.

After a secret was pushed and force-pushed away, purge the backups so the
commits are removed from the hub for good (and rotate the secret anyway).`,
	}
	type entry struct {
		Ref       string    `json:"ref"`
		Source    string    `json:"source"`
		Remote    string    `json:"remote"`
		Original  string    `json:"original"`
		SHA       string    `json:"sha"`
		Subject   string    `json:"subject"`
		CreatedAt time.Time `json:"created_at"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	list := &cobra.Command{
		Use: "list ORG/NAME", Aliases: []string{"ls"}, Short: "List backups", Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			p, err := repoPath(args[0])
			if err != nil {
				return err
			}
			var bs []entry
			if err := call(http.MethodGet, p+"/backups", nil, &bs); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(bs)
			}
			if len(bs) == 0 {
				fmt.Println("no backups")
				return nil
			}
			w := table()
			fmt.Fprintln(w, "CREATED\tFROM\tREF\tCOMMIT\tSUBJECT\tEXPIRES\tBACKUP")
			for _, b := range bs {
				from := b.Source
				if b.Remote != "" {
					from = b.Remote
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", ago(&b.CreatedAt), from, b.Original, short(b.SHA), oneLine(b.Subject), b.ExpiresAt.Format("2006-01-02"), b.Ref)
			}
			return w.Flush()
		},
	}
	var branch string
	restore := &cobra.Command{
		Use: "restore ORG/NAME BACKUP", Short: "Publish a backup as a new branch (mirrors receive it too)", Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			p, err := repoPath(args[0])
			if err != nil {
				return err
			}
			var res struct{ Branch, SHA string }
			if err := call(http.MethodPost, p+"/backups/restore", map[string]any{"backup": args[1], "branch": branch}, &res); err != nil {
				return err
			}
			fmt.Printf("restored %s as branch %s\n", short(res.SHA), res.Branch)
			return nil
		},
	}
	restore.Flags().StringVar(&branch, "branch", "", "branch name (default: restore/<branch>-<time>)")
	var all, purgeLFS, yes bool
	purge := &cobra.Command{
		Use:   "purge ORG/NAME [BACKUP...]",
		Short: "Delete backups and remove their content from the hub for good",
		Long: `Delete backups and remove the commits only they referenced from the hub
(git gc), so those commits can no longer be fetched, even by SHA. With
--lfs, Git LFS objects that nothing references any more are deleted too.
Content written in the last five minutes is kept; mirrors are not touched.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			p, err := repoPath(args[0])
			if err != nil {
				return err
			}
			if len(args) == 1 && !all {
				return errors.New("name the backups to purge, or pass --all")
			}
			if !yes {
				return errors.New("this cannot be undone; add --yes to confirm")
			}
			var res struct {
				Backups    int `json:"backups"`
				LFSObjects int `json:"lfs_objects"`
			}
			if err := call(http.MethodPost, p+"/backups/purge", map[string]any{"backups": args[1:], "all": all, "lfs": purgeLFS}, &res); err != nil {
				return err
			}
			fmt.Printf("purged %d backup(s)", res.Backups)
			if purgeLFS {
				fmt.Printf(" and %d unreferenced LFS object(s)", res.LFSObjects)
			}
			fmt.Println(". Mirrors keep their own copies; rotate any leaked secret.")
			return nil
		},
	}
	purge.Flags().BoolVar(&all, "all", false, "purge every backup of the repository")
	purge.Flags().BoolVar(&purgeLFS, "lfs", false, "also delete LFS objects no ref references any more")
	purge.Flags().BoolVar(&yes, "yes", false, "confirm")
	cmd.AddCommand(list, restore, purge)
	return cmd
}

// ------------------------------------------------------------------- audit

func auditCmd() *cobra.Command {
	var repo, actor, action string
	var limit int
	var before int64
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Show the audit log (admin changes and pushes)",
		Long: `Show the audit log, newest first.

Every API call that changes something (with its status, also when refused)
and every push accepted by the hub is recorded with who did it, from which
address and over which transport. Request bodies are never recorded.
Entries are kept for audit.retention (default 365 days).`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			q := url.Values{}
			if repo != "" {
				q.Set("repo", strings.TrimSuffix(strings.Trim(repo, "/"), ".git"))
			}
			if actor != "" {
				q.Set("actor", actor)
			}
			if action != "" {
				q.Set("action", action)
			}
			if before > 0 {
				q.Set("before", strconv.FormatInt(before, 10))
			}
			q.Set("limit", strconv.Itoa(limit))
			var page struct {
				Entries []struct {
					ID         int64     `json:"id"`
					At         time.Time `json:"at"`
					Actor      string    `json:"actor"`
					Via        string    `json:"via"`
					ClientAddr string    `json:"client_addr"`
					Action     string    `json:"action"`
					Path       string    `json:"path"`
					Repo       string    `json:"repo"`
					Status     int       `json:"status"`
					Detail     string    `json:"detail"`
				} `json:"entries"`
				Next int64 `json:"next"`
			}
			if err := call(http.MethodGet, "/api/audit?"+q.Encode(), nil, &page); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(page)
			}
			if len(page.Entries) == 0 {
				fmt.Println("no entries")
				return nil
			}
			w := table()
			fmt.Fprintln(w, "ID\tTIME\tACTOR\tVIA\tCLIENT\tACTION\tSTATUS\tDETAIL")
			for _, e := range page.Entries {
				what := e.Action
				if e.Path != "" {
					// The route plus the concrete path reads best as
					// "DELETE /api/targets/mirror-a".
					what, _, _ = strings.Cut(e.Action, " ")
					what += " " + e.Path
				} else if e.Repo != "" {
					what += " " + e.Repo
				}
				status := "-"
				if e.Status != 0 {
					status = strconv.Itoa(e.Status)
				}
				fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.ID, e.At.Local().Format("2006-01-02 15:04:05"), oneLine(e.Actor), oneLine(e.Via), orDash(oneLine(e.ClientAddr)), oneLine(what), status, oneLine(e.Detail))
			}
			w.Flush()
			if page.Next != 0 {
				more := fmt.Sprintf("gr audit --before %d -n %d", page.Next, limit)
				for _, f := range [][2]string{{"repo", repo}, {"actor", actor}, {"action", action}} {
					if f[1] != "" {
						more += fmt.Sprintf(" --%s %q", f[0], f[1])
					}
				}
				fmt.Printf("\nolder entries: %s\n", more)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "only entries for ORG/NAME")
	cmd.Flags().StringVar(&actor, "actor", "", "only entries by this token, key (ssh:<name>) or admin")
	cmd.Flags().StringVar(&action, "action", "", "only actions containing this text, e.g. push, DELETE, targets")
	cmd.Flags().IntVarP(&limit, "limit", "n", 50, "number of entries (max 500)")
	cmd.Flags().Int64Var(&before, "before", 0, "only entries older than this ID (paging)")
	return cmd
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ----------------------------------------------------------------- release

type releaseView struct {
	Tag        string `json:"tag"`
	Name       string `json:"name"`
	Body       string `json:"body"`
	Prerelease bool   `json:"prerelease"`
	Published  bool   `json:"published"`
	Assets     []struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
		URL    string `json:"url"`
	} `json:"assets"`
	Mirrors []struct {
		Mirror string `json:"mirror"`
		State  string `json:"state"`
		Error  string `json:"error"`
	} `json:"mirrors"`
	CreatedAt time.Time `json:"created_at"`
}

func releasePath(slug, tag string) (string, error) {
	p, err := repoPath(slug)
	if err != nil {
		return "", err
	}
	if tag == "" {
		return p + "/releases", nil
	}
	return p + "/releases/" + url.PathEscape(tag), nil
}

func printRelease(r *releaseView) error {
	if flagJSON {
		return printJSON(r)
	}
	kind := ""
	if r.Prerelease {
		kind = " (pre-release)"
	}
	if !r.Published {
		kind += " (draft: not published to mirrors)"
	}
	fmt.Printf("%s  %s%s\n", r.Tag, r.Name, kind)
	for _, a := range r.Assets {
		fmt.Printf("  file  %-32s %10d  sha256:%s\n", a.Name, a.Size, a.SHA256[:16])
	}
	w := table()
	for _, m := range r.Mirrors {
		fmt.Fprintf(w, "  %s\t%s\t%s\n", m.Mirror, m.State, oneLine(m.Error))
	}
	return w.Flush()
}

func readNotes(notes, file string) (*string, error) {
	switch {
	case file == "-":
		b, err := io.ReadAll(os.Stdin)
		s := string(b)
		return &s, err
	case file != "":
		b, err := os.ReadFile(file)
		s := string(b)
		return &s, err
	case notes != "":
		return &notes, nil
	}
	return nil, nil
}

func uploadFiles(path string, files []string) error {
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			return err
		}
		info, err := fh.Stat()
		if err != nil {
			fh.Close()
			return err
		}
		ct := mime.TypeByExtension(filepath.Ext(f))
		if ct == "" {
			ct = "application/octet-stream"
		}
		fmt.Fprintf(os.Stderr, "uploading %s (%d bytes)…\n", filepath.Base(f), info.Size())
		err = api().Upload(context.Background(), http.MethodPut, path+"/assets/"+url.PathEscape(filepath.Base(f)), ct, fh, info.Size(), nil)
		fh.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
	}
	return nil
}

func publishAndReport(path string, adopt, force bool) error {
	body := map[string]any{"wait": true, "force": force}
	if adopt {
		body["adopt"] = true
	}
	var r releaseView
	if err := call(http.MethodPost, path+"/publish", body, &r); err != nil {
		return err
	}
	if err := printRelease(&r); err != nil {
		return err
	}
	for _, m := range r.Mirrors {
		if m.State == "failed" || m.State == "conflict" {
			return fmt.Errorf("not published to every mirror; failed mirrors are retried automatically")
		}
	}
	return nil
}

func releaseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "release",
		Short: "Releases kept on the hub and published to every mirror",
		Long: `Releases kept on the hub and published to every mirror.

A release (notes and files) is created once on Gitrepo and published to the
GitHub, GitLab, Gitea/Forgejo and Gitee mirrors of the repository. It reaches
a mirror once the tag has been synced there. Releases that already exist on
a platform and were not made by Gitrepo are left alone unless --adopt.

These commands need a token with write access to the repository (not admin),
so CI can publish releases:

  gr release create acme/app v1.2.0 --notes-file CHANGELOG.md dist/*`,
	}

	var title, notes, notesFile string
	var pre, draft, adopt bool
	create := &cobra.Command{
		Use: "create ORG/NAME TAG [FILE...]", Short: "Create a release, attach files and publish it", Args: cobra.MinimumNArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			base, _ := releasePath(args[0], "")
			path, err := releasePath(args[0], args[1])
			if err != nil {
				return err
			}
			body := map[string]any{"tag": args[1], "prerelease": pre, "publish": false}
			if title != "" {
				body["name"] = title
			}
			n, err := readNotes(notes, notesFile)
			if err != nil {
				return err
			}
			if n != nil {
				body["body"] = *n
			}
			if err := call(http.MethodPost, base, body, nil); err != nil {
				return err
			}
			if err := uploadFiles(path, args[2:]); err != nil {
				return err
			}
			if draft {
				var r releaseView
				if err := call(http.MethodGet, path, nil, &r); err != nil {
					return err
				}
				return printRelease(&r)
			}
			return publishAndReport(path, adopt, false)
		},
	}
	create.Flags().StringVarP(&title, "title", "t", "", "release title (default: the tag)")
	create.Flags().StringVarP(&notes, "notes", "n", "", "release notes")
	create.Flags().StringVarP(&notesFile, "notes-file", "F", "", "read the notes from a file (- for stdin)")
	create.Flags().BoolVar(&pre, "prerelease", false, "mark as a pre-release")
	create.Flags().BoolVar(&draft, "draft", false, "keep it on the hub only; publish later with gr release publish")
	create.Flags().BoolVar(&adopt, "adopt", false, "take over releases that already exist on mirrors for this tag")

	var eTitle, eNotes, eNotesFile, ePre string
	edit := &cobra.Command{
		Use: "edit ORG/NAME TAG", Short: "Change title, notes or pre-release flag", Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			path, err := releasePath(args[0], args[1])
			if err != nil {
				return err
			}
			body := map[string]any{"wait": true}
			if c.Flags().Changed("title") {
				body["name"] = eTitle
			}
			n, err := readNotes(eNotes, eNotesFile)
			if err != nil {
				return err
			}
			if n != nil {
				body["body"] = *n
			}
			if ePre != "" {
				v, err := strconv.ParseBool(ePre)
				if err != nil {
					return fmt.Errorf("--prerelease: %w", err)
				}
				body["prerelease"] = v
			}
			var r releaseView
			if err := call(http.MethodPatch, path, body, &r); err != nil {
				return err
			}
			return printRelease(&r)
		},
	}
	edit.Flags().StringVarP(&eTitle, "title", "t", "", "new title")
	edit.Flags().StringVarP(&eNotes, "notes", "n", "", "new notes")
	edit.Flags().StringVarP(&eNotesFile, "notes-file", "F", "", "read the new notes from a file (- for stdin)")
	edit.Flags().StringVar(&ePre, "prerelease", "", "true or false")

	upload := &cobra.Command{
		Use: "upload ORG/NAME TAG FILE...", Short: "Attach (or replace) files", Args: cobra.MinimumNArgs(3),
		RunE: func(c *cobra.Command, args []string) error {
			path, err := releasePath(args[0], args[1])
			if err != nil {
				return err
			}
			if err := uploadFiles(path, args[2:]); err != nil {
				return err
			}
			var r releaseView
			if err := call(http.MethodGet, path, nil, &r); err != nil {
				return err
			}
			if r.Published {
				return publishAndReport(path, false, false)
			}
			return printRelease(&r)
		},
	}
	removeFile := &cobra.Command{
		Use: "remove-file ORG/NAME TAG NAME", Short: "Remove an attached file", Args: cobra.ExactArgs(3),
		RunE: func(c *cobra.Command, args []string) error {
			path, err := releasePath(args[0], args[1])
			if err != nil {
				return err
			}
			if err := call(http.MethodDelete, path+"/assets/"+url.PathEscape(args[2]), nil, nil); err != nil {
				return err
			}
			var r releaseView
			if err := call(http.MethodGet, path, nil, &r); err != nil {
				return err
			}
			if r.Published {
				return publishAndReport(path, false, false)
			}
			return printRelease(&r)
		},
	}
	var pAdopt, pForce bool
	publish := &cobra.Command{
		Use: "publish ORG/NAME TAG", Short: "Publish (or re-publish) to every mirror", Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			path, err := releasePath(args[0], args[1])
			if err != nil {
				return err
			}
			return publishAndReport(path, pAdopt, pForce)
		},
	}
	publish.Flags().BoolVar(&pAdopt, "adopt", false, "take over releases that already exist on mirrors for this tag")
	publish.Flags().BoolVar(&pForce, "force", false, "send it again even to mirrors that are up to date (e.g. deleted there by hand)")

	list := &cobra.Command{
		Use: "list ORG/NAME", Aliases: []string{"ls"}, Short: "List releases", Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			path, err := releasePath(args[0], "")
			if err != nil {
				return err
			}
			var rs []releaseView
			if err := call(http.MethodGet, path, nil, &rs); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(rs)
			}
			if len(rs) == 0 {
				fmt.Println("no releases")
				return nil
			}
			w := table()
			fmt.Fprintln(w, "TAG\tTITLE\tFILES\tMIRRORS\tCREATED")
			for _, r := range rs {
				counts := map[string]int{}
				for _, m := range r.Mirrors {
					counts[m.State]++
				}
				var parts []string
				for _, st := range []string{"synced", "waiting", "pending", "draft", "failed", "conflict", "unsupported", "paused"} {
					if counts[st] > 0 {
						parts = append(parts, fmt.Sprintf("%d %s", counts[st], st))
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", oneLine(r.Tag), oneLine(r.Name), len(r.Assets), strings.Join(parts, ", "), ago(&r.CreatedAt))
			}
			return w.Flush()
		},
	}
	show := &cobra.Command{
		Use: "show ORG/NAME TAG", Short: "Show a release and its state on each mirror", Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			path, err := releasePath(args[0], args[1])
			if err != nil {
				return err
			}
			var r releaseView
			if err := call(http.MethodGet, path, nil, &r); err != nil {
				return err
			}
			return printRelease(&r)
		},
	}
	var output string
	download := &cobra.Command{
		Use: "download ORG/NAME TAG NAME", Short: "Download an attached file from the hub", Args: cobra.ExactArgs(3),
		RunE: func(c *cobra.Command, args []string) error {
			path, err := releasePath(args[0], args[1])
			if err != nil {
				return err
			}
			if output == "" {
				output = filepath.Base(args[2])
			}
			out := os.Stdout
			if output != "-" {
				if out, err = os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644); err != nil {
					return err
				}
				defer out.Close()
			}
			if err := api().Download(context.Background(), path+"/assets/"+url.PathEscape(args[2]), out); err != nil {
				if output != "-" {
					os.Remove(output)
				}
				return err
			}
			return nil
		},
	}
	download.Flags().StringVarP(&output, "output", "o", "", "output file (- for stdout; default: the file name)")
	var hubOnly bool
	rm := &cobra.Command{
		Use: "rm ORG/NAME TAG", Short: "Delete a release from the mirrors and the hub", Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			path, err := releasePath(args[0], args[1])
			if err != nil {
				return err
			}
			if hubOnly {
				path += "?hub_only=true"
			}
			var res struct {
				Results []struct {
					Mirror string `json:"mirror"`
					State  string `json:"state"`
					Error  string `json:"error"`
				} `json:"results"`
			}
			err = call(http.MethodDelete, path, nil, &res)
			for _, r := range res.Results {
				fmt.Printf("  %s: %s %s\n", r.Mirror, r.State, oneLine(r.Error))
			}
			return err
		},
	}
	rm.Flags().BoolVar(&hubOnly, "hub-only", false, "delete only on the hub; leave the platforms' releases alone")
	cmd.AddCommand(create, edit, upload, removeFile, publish, list, show, download, rm)
	return cmd
}
