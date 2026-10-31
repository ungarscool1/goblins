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
	"github.com/ungarscool1/goblins/internal/providers/fake"
)

const usage = `goblins - provider-agnostic autonomous software engineering orchestration

Usage:
  goblins serve [--config FILE | --mock]     Run the service
  goblins config check [--config FILE|--mock] Validate configuration
  goblins config render [--config FILE|--mock] Print effective configuration, secrets masked
  goblins doctor [--config FILE|--mock]      Check providers and agent login, using no model
  goblins status [--config FILE|--mock]      List tasks
  goblins comment --task ID --as NAME --body TEXT [--config FILE|--mock]
                                             Enter a comment by hand, for trackers without one
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
		cfg, mock, err := loadFlags("serve", args[1:], stderr, nil)
		if err != nil {
			return err
		}
		log, err := observability.NewLogger(stderr, observability.LogOptions{Level: cfg.Log.Level, Format: cfg.Log.Format})
		if err != nil {
			return err
		}
		return app.Run(ctx, cfg, log, appOptions(mock))
	case "doctor":
		cfg, mock, err := loadFlags("doctor", args[1:], stderr, nil)
		if err != nil {
			return err
		}
		log, err := observability.NewLogger(stderr, observability.LogOptions{Level: "warn", Format: "text"})
		if err != nil {
			return err
		}
		store, err := app.OpenStore(ctx, ensureDir(cfg))
		if err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
		sys, err := app.Build(cfg, store, log, appOptions(mock))
		if err != nil {
			return err
		}
		if mock != nil {
			for _, p := range cfg.Projects {
				mock.SetBranch(p.Repository, "main", "1111111111111111111111111111111111111111")
			}
		}
		if err := app.Doctor(ctx, sys, func(f string, a ...any) { _, _ = fmt.Fprintf(stdout, f, a...) }); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(stdout, "ok")
		return nil
	case "status":
		cfg, _, err := loadFlags("status", args[1:], stderr, nil)
		if err != nil {
			return err
		}
		store, err := app.OpenStore(ctx, ensureDir(cfg))
		if err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
		tasks, err := store.ListTasks(ctx, "")
		if err != nil {
			return err
		}
		for _, t := range tasks {
			_, _ = fmt.Fprintf(stdout, "%s\t%s\t%s\t%s", t.ID, t.Project, t.State, t.Title)
			if t.Reason != "" {
				_, _ = fmt.Fprintf(stdout, "\t(%s)", t.Reason)
			}
			_, _ = fmt.Fprintln(stdout)
		}
		return nil
	case "comment":
		var task, as, body string
		cfg, _, err := loadFlags("comment", args[1:], stderr, func(fs *flag.FlagSet) {
			fs.StringVar(&task, "task", "", "task ID")
			fs.StringVar(&as, "as", "", "your name, recorded as the author")
			fs.StringVar(&body, "body", "", "comment text")
		})
		if err != nil {
			return err
		}
		if task == "" || as == "" || body == "" {
			return errors.New("--task, --as and --body are required")
		}
		store, err := app.OpenStore(ctx, ensureDir(cfg))
		if err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
		return app.OperatorComment(ctx, store, task, as, body, time.Now().UTC())
	case "config":
		if len(args) < 2 {
			return errors.New("usage: goblins config check|render")
		}
		cfg, _, err := loadFlags("config "+args[1], args[2:], stderr, nil)
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

// loadFlags returns the configuration, plus the fake world behind it in mock mode.
func loadFlags(name string, args []string, stderr io.Writer, extra func(*flag.FlagSet)) (config.Config, *fake.World, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "configuration file (or GOBLINS_CONFIG)")
	mock := fs.Bool("mock", false, "use the built-in mock configuration")
	if extra != nil {
		extra(fs)
	}
	if err := fs.Parse(args); err != nil {
		return config.Config{}, nil, err
	}
	if *mock && *path != "" {
		return config.Config{}, nil, errors.New("--mock and --config are mutually exclusive")
	}
	world := fake.NewWorld()
	reg := builtin.Registry(world)
	switch {
	case *mock:
		cfg, err := config.LoadBytes(config.MockYAML, os.LookupEnv, reg.Known)
		return cfg, world, err
	case *path == "":
		*path = os.Getenv("GOBLINS_CONFIG")
		if *path == "" {
			return config.Config{}, nil, errors.New("--config or --mock is required")
		}
	}
	cfg, err := config.Load(*path, os.LookupEnv, reg.Known)
	return cfg, nil, err
}

// appOptions shares one world between mock mode's routes and its adapters.
func appOptions(world *fake.World) app.Options {
	if world == nil {
		return app.Options{Registry: builtin.Registry(fake.NewWorld())}
	}
	return app.Options{Registry: builtin.Registry(world), Mock: world}
}

func ensureDir(cfg config.Config) config.Config {
	_ = os.MkdirAll(cfg.Installation.DataDir, 0o700)
	return cfg
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
