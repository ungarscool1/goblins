package app

import (
	"slices"

	"github.com/ungarscool1/goblins/internal/config"
	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/memory"
	"github.com/ungarscool1/goblins/internal/pipeline"
	"github.com/ungarscool1/goblins/internal/providers"
)

func memoryFor(b *builder, cfg config.Config, pc config.Project,
	scm core.SourceControl) (*pipeline.Memory, error) {
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
	if len(pc.Memory.Repository) > 0 {
		reader, ok := scm.(core.RepositoryReader)
		if !ok || !scm.Info().Has(core.CapSCMFiles) {
			return nil, core.Unsupported(scm.Info(), core.CapSCMFiles)
		}
		m.Repository = &pipeline.RepositoryDocs{Reader: reader, Globs: pc.Memory.Repository,
			Required: slices.Contains(pc.Memory.Required, "repo"), Cache: &memory.BlobCache{}}
	}
	return m, nil
}
