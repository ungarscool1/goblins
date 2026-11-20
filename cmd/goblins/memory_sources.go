package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/ungarscool1/goblins/internal/app"
	"github.com/ungarscool1/goblins/internal/config"
	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/memory"
	"github.com/ungarscool1/goblins/internal/pipeline"
)

// showMemory prints one memory of the namespace, with its provenance.
func showMemory(ctx context.Context, id, ns string, store core.StateStore, out io.Writer) error {
	if id == "" {
		return errors.New("--id is required")
	}
	m, err := store.GetMemory(ctx, id)
	if err != nil {
		return err
	}
	if m.Namespace != ns {
		return fmt.Errorf("memory %s is not in %s", id, ns)
	}
	when := func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return t.UTC().Format(time.RFC3339)
	}
	_, _ = fmt.Fprintf(out, "%s: %s\nkind %s, %s, in %s\ncreated by %s %s, accepted by %s, "+
		"verified %s, expires %s\n", m.ID, m.Title, m.Kind, m.Status, m.Namespace,
		cmp.Or(m.CreatedBy, "?"), when(m.CreatedAt), cmp.Or(m.ApprovedBy, "nobody"),
		when(m.VerifiedAt), when(m.ExpiresAt))
	if len(m.Tags) > 0 {
		_, _ = fmt.Fprintf(out, "tags: %s\n", strings.Join(m.Tags, ", "))
	}
	for _, s := range m.Sources {
		_, _ = fmt.Fprintf(out, "source: %s\n", s)
	}
	_, _ = fmt.Fprintf(out, "\n%s\n", m.Body)
	return nil
}

// memorySources asks each knowledge source of a project, as a run would, and says what came
// back: a way to check access, scopes and what agents would read, without running a task.
func memorySources(ctx context.Context, cfg config.Config, o app.Options,
	store core.StateStore, f memoryFlags, out io.Writer) error {
	if f.project == "" {
		return errors.New("--project is required")
	}
	sys, err := app.Build(cfg, store, slog.New(slog.DiscardHandler), o)
	if err != nil {
		return err
	}
	pr, ok := sys.Projects[f.project]
	if !ok {
		return fmt.Errorf("no project %s", f.project)
	}
	if pr.Memory == nil {
		_, _ = fmt.Fprintf(out, "memory is off for project %s\n", f.project)
		return nil
	}
	srcs, err := pipeline.MemorySources(ctx, pr)
	if err != nil {
		return err
	}
	terms := memory.Terms(cmp.Or(f.text, pr.ID))
	if len(srcs) == 0 {
		_, _ = fmt.Fprintln(out, "no knowledge sources: agents get accepted memories only")
	}
	failed := 0
	for _, s := range srcs {
		need := "optional"
		if s.Required {
			need = "required"
		}
		docs, err := s.Source.Search(ctx, core.KnowledgeQuery{Terms: terms, Limit: 5})
		if err != nil {
			failed++
			_, _ = fmt.Fprintf(out, "%s\t%s\t%s\tfailed: %s\n", s.Name, s.Source.Info().Type,
				need, core.UserMessage(err))
			continue
		}
		_, _ = fmt.Fprintf(out, "%s\t%s\t%s\tok, %d documents for %q\n", s.Name,
			s.Source.Info().Type, need, len(docs), strings.Join(terms, " "))
		for _, d := range docs {
			_, _ = fmt.Fprintf(out, "\t%s:%s\t%s\t%s\n", s.Name, d.ID, d.Title, d.Revision)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d knowledge sources failed", failed)
	}
	return nil
}
