package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/memory"
	"github.com/ungarscool1/goblins/internal/scheduler"
)

// Memory is a project's memory: its knowledge sources and how much of a prompt they may take.
type Memory struct {
	Sources []memory.Source
	Budget  int
}

// namespaces are where a project's memories live: its own, and its installation's. Nothing of
// another project or installation is ever read.
func (p *Pipeline) namespaces(project string) []string {
	return []string{p.Installation + "/" + project, p.Installation}
}

// addMemory gives a run the project's most relevant accepted memories and documents, as data,
// and records what it gave. A required source that fails stops the run, visibly.
func (p *Pipeline) addMemory(ctx context.Context, jc scheduler.JobContext, role core.AgentRole,
	req *core.AgentRequest) error {
	pr, ok := p.Projects[jc.Task.Project]
	if !ok || pr.Memory == nil || role == core.RoleTriager {
		return nil
	}
	text := jc.Task.Title + "\n" + req.Prompt[:min(len(req.Prompt), 6000)]
	pack, err := memory.Build(ctx, memory.Request{Namespaces: p.namespaces(jc.Task.Project),
		Store: p.Memories, Sources: pr.Memory.Sources, Text: text, Budget: pr.Memory.Budget,
		Now: time.Now()})
	if err != nil {
		return err
	}
	for _, w := range pack.Warnings {
		jc.Log("memory", w)
	}
	if len(pack.Items) == 0 {
		return nil
	}
	jc.Log("memory", "given: "+strings.Join(pack.Refs(), ", "))
	req.Prompt += "\n\n" + dataBlock("project memory", newNonce(), pack.Text())
	return nil
}

// proposeMemory records what a published change did, as a proposal: nothing a model wrote
// becomes something agents are told until a person accepts it.
func (p *Pipeline) proposeMemory(ctx context.Context, jc scheduler.JobContext, pr *Project,
	design core.Design, pull core.PullRequest, sha string) {
	if pr.Memory == nil || p.Memories == nil {
		return
	}
	summary := strings.ToValidUTF8(design.Body[:min(len(design.Body), 1500)], "")
	now := time.Now()
	m := core.MemoryRecord{ID: fmt.Sprintf("p-%s-%s", jc.Task.ID, sha[:min(len(sha), 12)]),
		Namespace: p.Installation + "/" + jc.Task.Project, Kind: core.MemoryFix,
		Title: jc.Task.Title, Body: summary, Status: core.MemoryProposed, CreatedBy: "goblins",
		Sources:   []string{"task:" + jc.Task.ID, "commit:" + sha, "pull request:" + pull.URL},
		CreatedAt: now, UpdatedAt: now}
	if err := p.Memories.PutMemory(ctx, m); err != nil {
		p.Log.Warn("cannot propose a memory", "task", jc.Task.ID, "err", err)
	}
}
