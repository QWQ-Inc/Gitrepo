package main

import (
	"bufio"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type userInfo struct {
	Username    string     `json:"username"`
	Admin       bool       `json:"admin"`
	Scopes      []string   `json:"scopes"`
	Repos       []string   `json:"repos,omitempty"`
	Disabled    bool       `json:"disabled"`
	CreatedAt   *time.Time `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

// readPassword takes a password from an environment variable or stdin.
func readPassword(env string, stdin bool) (string, error) {
	switch {
	case env != "":
		v := os.Getenv(env)
		if v == "" {
			return "", fmt.Errorf("environment variable %s is empty", env)
		}
		return v, nil
	case stdin:
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", errors.New("no password on stdin")
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	return "", errors.New("give the password with --password-env VAR or --password-stdin")
}

func userCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Accounts: user name and password for the console and git over HTTPS",
		Long: `Accounts sign in to the console with a user name and password, and the same
password works for git over HTTPS (user name + password instead of a token).

The first account is created in the console with the bootstrap admin token
(GITREPO_ADMIN_TOKEN) and is an administrator. A forgotten password is reset
with that token too (console: 忘记密码), or by an administrator here.

A password is one secret for everything the account can do; prefer tokens
(gr token) for CI jobs and servers: they can be limited and rotated per use.`,
	}
	var admin bool
	var scopes, repos, pwEnv string
	var pwStdin bool
	add := &cobra.Command{
		Use:   "add NAME",
		Short: "Create an account",
		Example: `  gr user add alice --admin --password-env ALICE_PW
  printf '%s\n' "$PW" | gr user add ci-reader --scopes read --repos 'acme/*' --password-stdin`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			pw, err := readPassword(pwEnv, pwStdin)
			if err != nil {
				return err
			}
			body := map[string]any{"username": args[0], "password": pw, "admin": admin}
			if !admin {
				body["scopes"] = strings.Split(scopes, ",")
				body["repos"] = splitNonEmpty(repos)
			}
			var u userInfo
			if err := call(http.MethodPost, "/api/users", body, &u); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(u)
			}
			fmt.Printf("account %s created (%s)\n", u.Username, strings.Join(u.Scopes, ","))
			return nil
		},
	}
	add.Flags().BoolVar(&admin, "admin", false, "an administrator (console and API)")
	add.Flags().StringVar(&scopes, "scopes", "create", "read | write | create (each implies the previous); ignored with --admin")
	add.Flags().StringVar(&repos, "repos", "", "repository patterns, e.g. 'acme/*' (default: all)")
	add.Flags().StringVar(&pwEnv, "password-env", "", "read the password from this environment variable")
	add.Flags().BoolVar(&pwStdin, "password-stdin", false, "read the password from the first line of stdin")

	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List accounts", Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			var us []userInfo
			if err := call(http.MethodGet, "/api/users", nil, &us); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(us)
			}
			w := table()
			fmt.Fprintln(w, "NAME\tSCOPES\tREPOS\tSTATE\tLAST SIGN-IN")
			for _, u := range us {
				r := strings.Join(u.Repos, ",")
				if r == "" {
					r = "*"
				}
				state := "enabled"
				if u.Disabled {
					state = "disabled"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", u.Username, strings.Join(u.Scopes, ","), r, state, ago(u.LastLoginAt))
			}
			return w.Flush()
		},
	}

	var setAdmin, setUser, disable, enable bool
	var setScopes, setRepos string
	set := &cobra.Command{
		Use: "set NAME", Short: "Change an account's permissions or disable it (its sessions end)", Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			body := map[string]any{}
			if setAdmin && setUser {
				return errors.New("--admin and --no-admin are exclusive")
			}
			if setAdmin {
				body["admin"] = true
			}
			if setUser {
				body["admin"] = false
			}
			if c.Flags().Changed("scopes") {
				body["scopes"] = strings.Split(setScopes, ",")
			}
			if c.Flags().Changed("repos") {
				body["repos"] = splitNonEmpty(setRepos)
			}
			if disable && enable {
				return errors.New("--disable and --enable are exclusive")
			}
			if disable || enable {
				body["disabled"] = disable
			}
			if len(body) == 0 {
				return errors.New("nothing to change")
			}
			var u userInfo
			if err := call(http.MethodPatch, "/api/users/"+url.PathEscape(args[0]), body, &u); err != nil {
				return err
			}
			fmt.Printf("account %s: %s, disabled %v\n", u.Username, strings.Join(u.Scopes, ","), u.Disabled)
			return nil
		},
	}
	set.Flags().BoolVar(&setAdmin, "admin", false, "make an administrator")
	set.Flags().BoolVar(&setUser, "no-admin", false, "no longer an administrator")
	set.Flags().StringVar(&setScopes, "scopes", "", "read | write | create")
	set.Flags().StringVar(&setRepos, "repos", "", "repository patterns ('' = all)")
	set.Flags().BoolVar(&disable, "disable", false, "disable the account")
	set.Flags().BoolVar(&enable, "enable", false, "enable the account")

	var passEnv string
	var passStdin bool
	passwd := &cobra.Command{
		Use: "passwd NAME", Short: "Set an account's password (its sessions end)", Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			pw, err := readPassword(passEnv, passStdin)
			if err != nil {
				return err
			}
			if err := call(http.MethodPost, "/api/users/"+url.PathEscape(args[0])+"/password", map[string]string{"password": pw}, nil); err != nil {
				return err
			}
			fmt.Printf("password of %s set\n", args[0])
			return nil
		},
	}
	passwd.Flags().StringVar(&passEnv, "password-env", "", "read the password from this environment variable")
	passwd.Flags().BoolVar(&passStdin, "password-stdin", false, "read the password from the first line of stdin")

	rm := &cobra.Command{
		Use: "rm NAME", Short: "Delete an account", Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := call(http.MethodDelete, "/api/users/"+url.PathEscape(args[0]), nil, nil); err != nil {
				return err
			}
			fmt.Printf("account %s deleted\n", args[0])
			return nil
		},
	}
	cmd.AddCommand(add, list, set, passwd, rm)
	return cmd
}
