package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ungarscool1/goblins/internal/app"
	"github.com/ungarscool1/goblins/internal/config"
	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/memory"
)

const memoryUsage = `goblins memory COMMAND [flags] [--config FILE|--mock]

  list                         Memories of a project (or of the installation without --project)
  search --text TEXT           Accepted memories agents would be given for this text
  add --kind K --title T --body B [--source S]...
                               Add an accepted memory, as the person running the command (--as)
  accept --id ID               Accept a proposed memory, which agents are then given
  supersede --id ID --by ID    Mark a memory replaced by another
  forget --id ID               Delete a memory for good
  export                       All memories of the namespace, as JSON lines
  import                       Memories as JSON lines on standard input, into the namespace
`

type memoryFlags struct {
	project, id, by, text, kind, title, body, as string
	sources                                      multi
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// memoryCommand is the curation side of memory: people read, add, accept and forget what
// agents are told. Every change names who made it.
func memoryCommand(ctx context.Context, args []string, stdin io.Reader, stdout,
	stderr io.Writer) error {
	if len(args) == 0 {
		_, _ = io.WriteString(stderr, memoryUsage)
		return errors.New("missing memory command")
	}
	var f memoryFlags
	cfg, _, err := loadFlags("memory "+args[0], args[1:], stderr, func(fs *flag.FlagSet) {
		fs.StringVar(&f.project, "project", "", "project ID; none means installation-wide")
		fs.StringVar(&f.id, "id", "", "memory ID")
		fs.StringVar(&f.by, "by", "", "the memory replacing this one")
		fs.StringVar(&f.text, "text", "", "text to search with")
		fs.StringVar(&f.kind, "kind", "decision",
			"decision, rule, pitfall, fix, runbook or convention")
		fs.StringVar(&f.title, "title", "", "title")
		fs.StringVar(&f.body, "body", "", "body")
		fs.StringVar(&f.as, "as", os.Getenv("USER"), "who is making this change")
		fs.Var(&f.sources, "source", "where it comes from, such as a URL; repeatable")
	})
	if err != nil {
		return err
	}
	store, err := app.OpenStore(ctx, ensureDir(cfg))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	ns := cfg.Installation.Name
	if f.project != "" {
		if !hasProject(cfg, f.project) {
			return fmt.Errorf("no project %s", f.project)
		}
		ns += "/" + f.project
	}
	return runMemory(ctx, args[0], f, ns, store, stdin, stdout)
}

func hasProject(cfg config.Config, id string) bool {
	for _, p := range cfg.Projects {
		if p.ID == id {
			return true
		}
	}
	return false
}

func runMemory(ctx context.Context, cmd string, f memoryFlags, ns string, store core.StateStore,
	stdin io.Reader, stdout io.Writer) error {
	now := time.Now().UTC()
	switch cmd {
	case "list":
		all, err := store.ListMemories(ctx, []string{ns})
		for _, m := range all {
			_, _ = fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", m.ID, m.Status, m.Kind, m.Title)
		}
		return err
	case "search":
		pack, err := memory.Build(ctx, memory.Request{Namespaces: []string{ns}, Store: store,
			Text: f.text, Now: now})
		_, _ = io.WriteString(stdout, pack.Text())
		return err
	case "add":
		if f.title == "" || f.body == "" || f.as == "" {
			return errors.New("--title, --body and --as are required")
		}
		m := core.MemoryRecord{ID: "m-" + randomID(), Namespace: ns, Kind: core.MemoryKind(f.kind),
			Title: f.title, Body: f.body, Sources: f.sources, Status: core.MemoryAccepted,
			CreatedBy: f.as, ApprovedBy: f.as, CreatedAt: now, UpdatedAt: now, VerifiedAt: now}
		if err := store.PutMemory(ctx, m); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(stdout, m.ID)
		return nil
	case "accept", "supersede", "forget":
		return changeMemory(ctx, cmd, f, ns, store, now, stdout)
	case "export":
		all, err := store.ListMemories(ctx, []string{ns})
		enc := json.NewEncoder(stdout)
		for _, m := range all {
			if err := enc.Encode(m); err != nil {
				return err
			}
		}
		return err
	case "import":
		return importMemories(ctx, ns, store, stdin, stdout)
	}
	return fmt.Errorf("unknown memory command %q", cmd)
}

func changeMemory(ctx context.Context, cmd string, f memoryFlags, ns string,
	store core.StateStore, now time.Time, stdout io.Writer) error {
	m, err := store.GetMemory(ctx, f.id)
	if err != nil {
		return err
	}
	if m.Namespace != ns {
		return fmt.Errorf("memory %s is not in %s", f.id, ns)
	}
	switch cmd {
	case "forget":
		err = store.DeleteMemory(ctx, f.id)
	case "accept":
		if f.as == "" {
			return errors.New("--as is required")
		}
		m.Status, m.ApprovedBy, m.UpdatedAt, m.VerifiedAt = core.MemoryAccepted, f.as, now, now
		err = store.PutMemory(ctx, m)
	case "supersede":
		if f.by == "" {
			return errors.New("--by is required")
		}
		m.Status, m.UpdatedAt = core.MemorySuperseded, now
		m.Sources = append(m.Sources, "superseded by "+f.by)
		err = store.PutMemory(ctx, m)
	}
	if err == nil {
		_, _ = fmt.Fprintf(stdout, "%s %s\n", cmd, f.id)
	}
	return err
}

// importMemories reads memories into one namespace, whatever namespace a line names: an export
// from one installation never lands in another's.
func importMemories(ctx context.Context, ns string, store core.StateStore, in io.Reader,
	out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	n := 0
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var m core.MemoryRecord
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			return fmt.Errorf("line %d: %w", n+1, err)
		}
		m.Namespace = ns
		if err := store.PutMemory(ctx, m); err != nil {
			return err
		}
		n++
	}
	_, _ = fmt.Fprintf(out, "imported %d memories\n", n)
	return sc.Err()
}

func randomID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
