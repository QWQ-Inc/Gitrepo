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

// ssoFlags are the provider settings shared by add and set.
type ssoFlags struct {
	displayName, issuer, authURL, tokenURL, userinfoURL string
	clientID, clientSecretEnv, groupsClaim              string
	scopes, allow                                       []string
	stackenigma, clearSecret                            bool
}

func (f *ssoFlags) register(c *cobra.Command) {
	c.Flags().StringVar(&f.displayName, "display-name", "", "button text on the sign-in page (default: the name; StackEnigma for --stackenigma)")
	c.Flags().StringVar(&f.issuer, "issuer", "", "OpenID Connect issuer URL (endpoints are read from its discovery document)")
	c.Flags().StringVar(&f.authURL, "auth-url", "", "authorization endpoint (plain OAuth 2.0, or to override discovery)")
	c.Flags().StringVar(&f.tokenURL, "token-url", "", "token endpoint (plain OAuth 2.0, or to override discovery)")
	c.Flags().StringVar(&f.userinfoURL, "userinfo-url", "", "userinfo endpoint (plain OAuth 2.0, or to override discovery)")
	c.Flags().StringVar(&f.clientID, "client-id", "", "OAuth client ID registered with the provider")
	c.Flags().StringVar(&f.clientSecretEnv, "client-secret-env", "", "read the client secret from this environment variable")
	c.Flags().StringVar(&f.groupsClaim, "groups-claim", "", `claim holding the user's groups (default "groups")`)
	c.Flags().StringArrayVar(&f.scopes, "scope", nil, `scopes to request (repeatable; default "openid profile email")`)
	c.Flags().StringArrayVar(&f.allow, "allow", nil, `who may sign in (repeatable): "alice@example.com", "*@example.com", "group:NAME", "sub:SUBJECT" or "*"`)
	c.Flags().BoolVar(&f.stackenigma, "stackenigma", false, "StackEnigma preset (button and defaults)")
}

func (f *ssoFlags) body(c *cobra.Command) (map[string]any, error) {
	body := map[string]any{}
	str := func(flag, key, v string) {
		if c.Flags().Changed(flag) {
			body[key] = v
		}
	}
	str("display-name", "display_name", f.displayName)
	str("issuer", "issuer", f.issuer)
	str("auth-url", "auth_url", f.authURL)
	str("token-url", "token_url", f.tokenURL)
	str("userinfo-url", "userinfo_url", f.userinfoURL)
	str("client-id", "client_id", f.clientID)
	str("groups-claim", "groups_claim", f.groupsClaim)
	if c.Flags().Changed("scope") {
		body["scopes"] = f.scopes
	}
	if c.Flags().Changed("allow") {
		body["allow"] = f.allow
	}
	if f.stackenigma {
		body["kind"] = "stackenigma"
	}
	if f.clientSecretEnv != "" {
		sec := strings.TrimSpace(os.Getenv(f.clientSecretEnv))
		if sec == "" {
			return nil, fmt.Errorf("environment variable %s is empty", f.clientSecretEnv)
		}
		body["client_secret"] = sec
	}
	if f.clearSecret {
		body["clear_client_secret"] = true
	}
	return body, nil
}

func ssoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sso",
		Short: "Sign in to the console through StackEnigma or another OpenID Connect / OAuth 2.0 provider",
		Long: `Sign in to the console through an identity provider.

Register Gitrepo as an OAuth client (a "web application" with a client secret,
or a public client: PKCE is always used) and give it the callback URL that
gr sso add prints: <public_url>/auth/oidc/<name>/callback.

People who sign in this way are administrators of this Gitrepo, so list
exactly who may sign in with --allow. E-mail rules only match addresses the
provider marks as verified. Changing who may sign in, or disabling the
provider, signs out everyone who signed in through it.`,
	}
	var f ssoFlags
	add := &cobra.Command{
		Use:   "add NAME",
		Short: "Add a sign-in provider",
		Example: `  gr sso add stackenigma --stackenigma --issuer https://id.example.com --client-id gitrepo \
      --client-secret-env SE_CLIENT_SECRET --allow '*@example.com'
  gr sso add keycloak --issuer https://sso.example.com/realms/dev --client-id gitrepo \
      --client-secret-env KC_SECRET --allow group:gitrepo-admins
  gr sso add forge --auth-url https://git.example.com/login/oauth/authorize \
      --token-url https://git.example.com/login/oauth/access_token \
      --userinfo-url https://git.example.com/api/v1/user --client-id ID --client-secret-env SECRET --allow alice@example.com`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			body, err := f.body(c)
			if err != nil {
				return err
			}
			body["name"] = args[0]
			var p client.SSOProvider
			if err := call(http.MethodPost, "/api/sso/providers", body, &p); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(p)
			}
			fmt.Printf("sign-in provider %s added.\nRegister this callback URL with the provider: %s\nCheck it: gr sso check %s\n", p.Name, p.RedirectURL, p.Name)
			return nil
		},
	}
	f.register(add)

	var enable, disable bool
	set := &cobra.Command{
		Use:   "set NAME",
		Short: "Change a sign-in provider (only the flags given)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
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
			var p client.SSOProvider
			if err := call(http.MethodPatch, "/api/sso/providers/"+url.PathEscape(args[0]), body, &p); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(p)
			}
			fmt.Printf("sign-in provider %s updated\n", p.Name)
			return nil
		},
	}
	f.register(set)
	set.Flags().BoolVar(&f.clearSecret, "clear-client-secret", false, "remove the client secret (public client)")
	set.Flags().BoolVar(&enable, "enable", false, "show the provider on the sign-in page again")
	set.Flags().BoolVar(&disable, "disable", false, "hide the provider and sign out everyone who used it")

	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List sign-in providers", Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			var ps []client.SSOProvider
			if err := call(http.MethodGet, "/api/sso/providers", nil, &ps); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(ps)
			}
			w := table()
			fmt.Fprintln(w, "NAME\tBUTTON\tENDPOINTS\tCLIENT ID\tALLOW\tENABLED")
			for _, p := range ps {
				where := p.Issuer
				if where == "" {
					where = "OAuth 2.0 " + p.AuthURL
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%v\n", p.Name, p.DisplayName, where, p.ClientID, strings.Join(p.Allow, " "), p.Enabled)
			}
			return w.Flush()
		},
	}
	check := &cobra.Command{
		Use: "check NAME", Short: "Read the provider's endpoints and show the callback URL to register", Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			var r client.SSOCheck
			if err := call(http.MethodPost, "/api/sso/providers/"+url.PathEscape(args[0])+"/check", nil, &r); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(r)
			}
			fmt.Printf("mode:          %s\nauthorization: %s\ntoken:         %s\n", r.Mode, r.AuthURL, r.TokenURL)
			if r.UserinfoURL != "" {
				fmt.Printf("userinfo:      %s\n", r.UserinfoURL)
			}
			fmt.Printf("callback URL:  %s\n", r.RedirectURL)
			return nil
		},
	}
	rm := &cobra.Command{
		Use: "rm NAME", Short: "Remove a sign-in provider (signs out everyone who used it)", Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := call(http.MethodDelete, "/api/sso/providers/"+url.PathEscape(args[0]), nil, nil); err != nil {
				return err
			}
			fmt.Printf("sign-in provider %s removed\n", args[0])
			return nil
		},
	}
	sessions := &cobra.Command{
		Use: "sessions", Short: "List active console sign-ins", Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			var xs []client.Session
			if err := call(http.MethodGet, "/api/sso/sessions", nil, &xs); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(xs)
			}
			w := table()
			fmt.Fprintln(w, "ID\tWHO\tSIGNED IN\tEXPIRES\tLAST USED")
			for _, x := range xs {
				fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", x.ID, x.Name, ago(&x.CreatedAt), ago(&x.ExpiresAt), ago(x.LastUsedAt))
			}
			return w.Flush()
		},
	}
	signout := &cobra.Command{
		Use: "signout ID", Short: "End a console sign-in", Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := call(http.MethodDelete, "/api/sso/sessions/"+url.PathEscape(args[0]), nil, nil); err != nil {
				return err
			}
			fmt.Printf("session %s ended\n", args[0])
			return nil
		},
	}
	cmd.AddCommand(add, set, list, check, rm, sessions, signout)
	return cmd
}
