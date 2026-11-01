// Command gitrepo is the Gitrepo server. It also serves as the Git hook and
// GIT_ASKPASS helper, invoked by git through small shim scripts.
//
//	gitrepo serve [--config gitrepo.yaml]
//	gitrepo backup DEST | gitrepo restore SRC
//	gitrepo keygen
//	gitrepo version
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/qwq-inc/gitrepo/internal/config"
	"github.com/qwq-inc/gitrepo/internal/gitexec"
	"github.com/qwq-inc/gitrepo/internal/githooks"
	"github.com/qwq-inc/gitrepo/internal/secrets"
	"github.com/qwq-inc/gitrepo/internal/server"
	"github.com/qwq-inc/gitrepo/internal/snapshot"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(serve(os.Args[2:]))
	case "hook":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: gitrepo hook <pre-receive|post-receive>")
			os.Exit(2)
		}
		switch os.Args[2] {
		case "pre-receive":
			os.Exit(githooks.PreReceive(os.Stdin, os.Stderr))
		case "post-receive":
			os.Exit(githooks.PostReceive(os.Stdin, os.Stderr))
		default:
			// Unknown hooks succeed so future git hook types never block pushes.
			os.Exit(0)
		}
	case "askpass":
		prompt := strings.Join(os.Args[2:], " ")
		os.Exit(githooks.Askpass(prompt, os.Stdout, os.Stderr))
	case "keygen":
		fmt.Println(secrets.GenerateKey())
	case "backup":
		os.Exit(backup(os.Args[2:]))
	case "restore":
		os.Exit(restore(os.Args[2:]))
	case "healthcheck":
		os.Exit(healthcheck())
	case "version", "--version", "-v":
		fmt.Println("gitrepo", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Gitrepo — one Git URL that keeps GitHub, GitLab and self-hosted mirrors in sync

Usage:
  gitrepo serve [--config FILE] [--log-level info|debug]
  gitrepo keygen            print a new secret key for GITREPO_SECRET_KEY
  gitrepo backup [--config FILE] DEST
                            snapshot the instance (database, repositories, LFS, SSH host key)
                            into the new directory DEST; safe while the server runs
  gitrepo restore [--config FILE] SRC
                            recreate an instance from a snapshot in an empty data directory
  gitrepo healthcheck       exit 0 if the local server answers /healthz (for container health checks)
  gitrepo version

Environment:
  GITREPO_SECRET_KEY        32-byte key (base64/hex) encrypting platform credentials (required)
  GITREPO_ADMIN_TOKEN       bootstrap admin token for the API and the gr CLI
  GITREPO_LISTEN, GITREPO_PUBLIC_URL, GITREPO_DATA_DIR override the config file
`)
}

// healthcheck probes the local server on GITREPO_LISTEN (default :8080).
func healthcheck() int {
	addr := os.Getenv("GITREPO_LISTEN")
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck:", resp.Status)
		return 1
	}
	return 0
}

func serve(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", os.Getenv("GITREPO_CONFIG"), "path to gitrepo.yaml (optional)")
	logLevel := fs.String("log-level", "info", "log level: debug, info, warn, error")
	fs.Parse(args)

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintln(os.Stderr, "invalid --log-level:", err)
		return 2
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("load config", "err", err)
		return 1
	}
	srv, err := server.New(cfg, log, server.Options{})
	if err != nil {
		log.Error("start", "err", err)
		return 1
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Error("listen", "addr", cfg.Listen, "err", err)
		return 1
	}
	if err := srv.Start(ln); err != nil {
		log.Error("start", "err", err)
		return 1
	}
	log.Info("Gitrepo started", "version", version, "listen", cfg.Listen, "public_url", cfg.PublicURL, "data_dir", cfg.DataDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "err", err)
		return 1
	}
	return 0
}

// snapshotArgs parses [--config FILE] DIR for backup and restore.
func snapshotArgs(name string, args []string) (*config.Config, *gitexec.Git, string, bool) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	cfgPath := fs.String("config", os.Getenv("GITREPO_CONFIG"), "path to gitrepo.yaml (optional)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: gitrepo %s [--config FILE] DIR\n", name)
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		return nil, nil, "", false
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load config:", err)
		return nil, nil, "", false
	}
	g := &gitexec.Git{Bin: cfg.Git.Binary, Env: []string{"HOME=" + cfg.HomeDir(), "XDG_CONFIG_HOME=" + cfg.HomeDir()}, TmpDir: cfg.TmpDir()}
	return cfg, g, fs.Arg(0), true
}

func backup(args []string) int {
	cfg, g, dest, ok := snapshotArgs("backup", args)
	if !ok {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	m, err := snapshot.Create(ctx, cfg, g, dest, version, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "backup failed:", err)
		fmt.Fprintln(os.Stderr, "the incomplete snapshot has no manifest.json; delete", dest, "and try again")
		return 1
	}
	fmt.Fprintf(os.Stderr, "snapshot of %d repo(s) written to %s\n", len(m.Repos), dest)
	fmt.Fprintf(os.Stderr, "credentials in it are encrypted with %s: keep that key separately\n", cfg.SecretKeyEnv)
	return 0
}

func restore(args []string) int {
	cfg, g, src, ok := snapshotArgs("restore", args)
	if !ok {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	m, err := snapshot.Restore(ctx, cfg, g, src, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "restore failed:", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "restored %d repo(s) into %s (snapshot from %s)\n", len(m.Repos), cfg.DataDir, m.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(os.Stderr, "start the server with the same %s as the original instance\n", cfg.SecretKeyEnv)
	return 0
}
