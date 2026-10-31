// Command repohub is the repohub server. It also serves as the Git hook and
// GIT_ASKPASS helper, invoked by git through small shim scripts.
//
//	repohub serve [--config repohub.yaml]
//	repohub keygen
//	repohub version
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
	"github.com/qwq-inc/gitrepo/internal/githooks"
	"github.com/qwq-inc/gitrepo/internal/secrets"
	"github.com/qwq-inc/gitrepo/internal/server"
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
			fmt.Fprintln(os.Stderr, "usage: repohub hook <pre-receive|post-receive>")
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
	case "healthcheck":
		os.Exit(healthcheck())
	case "version", "--version", "-v":
		fmt.Println("repohub", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `repohub — one Git URL that keeps GitHub, GitLab and self-hosted mirrors in sync

Usage:
  repohub serve [--config FILE] [--log-level info|debug]
  repohub keygen            print a new secret key for REPOHUB_SECRET_KEY
  repohub healthcheck       exit 0 if the local server answers /healthz (for container health checks)
  repohub version

Environment:
  REPOHUB_SECRET_KEY        32-byte key (base64/hex) encrypting platform credentials (required)
  REPOHUB_ADMIN_TOKEN       bootstrap admin token for the API and the rh CLI
  REPOHUB_LISTEN, REPOHUB_PUBLIC_URL, REPOHUB_DATA_DIR override the config file
`)
}

// healthcheck probes the local server on REPOHUB_LISTEN (default :8080).
func healthcheck() int {
	addr := os.Getenv("REPOHUB_LISTEN")
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
	cfgPath := fs.String("config", os.Getenv("REPOHUB_CONFIG"), "path to repohub.yaml (optional)")
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
	log.Info("repohub started", "version", version, "listen", cfg.Listen, "public_url", cfg.PublicURL, "data_dir", cfg.DataDir)

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
