// Package app wires configuration, providers and services into a running
// process. It is the composition root; business logic lives elsewhere.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"

	"github.com/ungarscool1/goblins/internal/api"
	"github.com/ungarscool1/goblins/internal/config"
	"github.com/ungarscool1/goblins/internal/core"
)

// Version is overridden at build time with -ldflags.
var Version = "dev"

// Run starts the service and blocks until ctx is cancelled and shutdown ended.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	if err := checkDataDir(cfg.Installation.DataDir); err != nil {
		return err
	}
	if rendered, err := cfg.Render(); err == nil {
		log.Debug("effective configuration", "config", string(rendered))
	}

	srv := api.NewServer(log, Version, []api.Check{
		{Name: "data_dir", Run: func(context.Context) error { return checkDataDir(cfg.Installation.DataDir) }},
	})

	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return core.Wrap(core.KindInvalid, "app.listen", err)
	}
	log.Info("goblins started", "version", Version, "installation", cfg.Installation.Name, "listen", ln.Addr().String(), "projects", len(cfg.Projects))
	srv.SetReady(true)
	err = srv.Serve(ctx, ln, cfg.Server.ShutdownTimeout)
	log.Info("goblins stopped")
	return err
}

// checkDataDir verifies the data directory exists and is writable, creating it
// with owner-only permissions if needed.
func checkDataDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return core.Wrap(core.KindInvalid, "app.data_dir", err)
	}
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return core.Wrap(core.KindInvalid, "app.data_dir", fmt.Errorf("not writable: %w", err))
	}
	_ = f.Close()
	return os.Remove(filepath.Clean(f.Name()))
}
