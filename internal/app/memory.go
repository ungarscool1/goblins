package app

import (
	"slices"

	"github.com/ungarscool1/goblins/internal/config"
	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/memory"
	"github.com/ungarscool1/goblins/internal/pipeline"
	"github.com/ungarscool1/goblins/internal/providers"
)

func memoryFor(b *builder, cfg config.Config, pc config.Project) (*pipeline.Memory, error) {
	if !pc.Memory.Enabled {
		return nil, nil
	}
	m := &pipeline.Memory{Budget: pc.Memory.Budget}
	for _, name := range pc.Memory.Sources {
		src, err := build[core.KnowledgeSource](b, providers.KindKnowledge, cfg.Knowledge, name)
		if err != nil {
			return nil, err
		}
		m.Sources = append(m.Sources, memory.Source{Name: name, Source: src,
			Required: slices.Contains(pc.Memory.Required, name)})
	}
	return m, nil
}
