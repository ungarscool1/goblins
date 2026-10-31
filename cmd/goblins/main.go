// Command goblins runs the Goblins orchestration service and its tooling.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ungarscool1/goblins/internal/app"
	"github.com/ungarscool1/goblins/internal/config"
	"github.com/ungarscool1/goblins/internal/observability"
	"github.com/ungarscool1/goblins/internal/providers/builtin"
)

const usage = `goblins - provider-agnostic autonomous software engineering orchestration

Usage:
  goblins serve [--config FILE | --mock]     Run the service
  goblins config check [--config FILE|--mock] Validate configuration
  goblins config render [--config FILE|--mock] Print effective configuration, secrets masked
  goblins healthcheck [--url URL]            Probe a running instance (container health)
  goblins version                            Print the version
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, _ = io.WriteString(stderr, usage)
		return errors.New("missing command")
	}
	switch args[0] {
	case "serve":
		cfg, err := loadFlags("serve", args[1:], stderr)
		if err != nil {
			return err
		}
		log, err := observability.NewLogger(stderr, observability.LogOptions{Level: cfg.Log.Level, Format: cfg.Log.Format})
		if err != nil {
			return err
		}
		return app.Run(ctx, cfg, log)
	case "config":
		if len(args) < 2 {
			return errors.New("usage: goblins config check|render")
		}
		cfg, err := loadFlags("config "+args[1], args[2:], stderr)
		if err != nil {
			return err
		}
		switch args[1] {
		case "check":
			_, _ = fmt.Fprintln(stdout, "configuration is valid")
			return nil
		case "render":
			out, err := cfg.Render()
			if err != nil {
				return err
			}
			_, err = stdout.Write(out)
			return err
		}
		return fmt.Errorf("unknown config command %q", args[1])
	case "healthcheck":
		return healthcheck(ctx, args[1:], stderr)
	case "version":
		_, _ = fmt.Fprintln(stdout, app.Version)
		return nil
	case "help", "-h", "--help":
		_, _ = io.WriteString(stdout, usage)
		return nil
	}
	_, _ = io.WriteString(stderr, usage)
	return fmt.Errorf("unknown command %q", args[0])
}

func loadFlags(name string, args []string, stderr io.Writer) (config.Config, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", os.Getenv("GOBLINS_CONFIG"), "configuration file")
	mock := fs.Bool("mock", false, "use the built-in mock configuration")
	if err := fs.Parse(args); err != nil {
		return config.Config{}, err
	}
	reg := builtin.Registry()
	switch {
	case *mock && *path != "" && os.Getenv("GOBLINS_CONFIG") != *path:
		return config.Config{}, errors.New("--mock and --config are mutually exclusive")
	case *mock:
		return config.LoadBytes(config.MockYAML, os.LookupEnv, reg.Known)
	case *path == "":
		return config.Config{}, errors.New("--config or --mock is required")
	}
	return config.Load(*path, os.LookupEnv, reg.Known)
}

func healthcheck(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "http://127.0.0.1:8080/readyz", "probe URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy: HTTP %d", resp.StatusCode)
	}
	return nil
}
