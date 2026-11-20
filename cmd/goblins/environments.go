package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ungarscool1/goblins/internal/app"
	"github.com/ungarscool1/goblins/internal/pipeline"
)

// environments lists development environment leases and, with --release, gives one back. It is
// how a person ends the hold on a target whose reset failed, once they restored it.
func environments(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var release string
	cfg, _, err := loadFlags("environments", args, stderr, func(fs *flag.FlagSet) {
		fs.StringVar(&release, "release", "", "release this lease, such as env:dev:dev-1")
	})
	if err != nil {
		return err
	}
	store, err := app.OpenStore(ctx, ensureDir(cfg))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	if release != "" {
		l, err := store.GetLease(ctx, release)
		if err != nil {
			return err
		}
		if err := store.ReleaseLease(ctx, release, l.Holder); err != nil {
			return err
		}
		// Released by hand, the target was restored: nobody needs to reset it for its holder.
		if d, err := store.GetLease(ctx, pipeline.DirtyResource(release)); err == nil {
			if err := store.ReleaseLease(ctx, d.Resource, d.Holder); err != nil {
				return err
			}
		}
		_, _ = fmt.Fprintf(stdout, "released %s, held by task %s since %s\n", release, l.Holder,
			l.AcquiredAt.UTC().Format(time.RFC3339))
		return nil
	}
	leases, err := store.ListLeases(ctx, "env:")
	if err != nil {
		return err
	}
	for _, l := range leases {
		var notes []string
		for _, k := range slices.Sorted(maps.Keys(l.Data)) {
			notes = append(notes, k+"="+l.Data[k])
		}
		_, _ = fmt.Fprintf(stdout, "%s\ttask %s\tsince %s\tuntil %s\t%s\n", l.Resource, l.Holder,
			l.AcquiredAt.UTC().Format(time.RFC3339), l.ExpiresAt.UTC().Format(time.RFC3339),
			strings.Join(notes, " "))
	}
	return nil
}
