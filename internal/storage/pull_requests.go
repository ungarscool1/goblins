package storage

import (
	"context"

	"github.com/ungarscool1/goblins/internal/core"
)

// FindTaskByPullRequest implements core.StateStore. Signals about pull requests are rare and
// unfinished tasks few, so this reads those rather than maintaining an index.
func (s *Store) FindTaskByPullRequest(ctx context.Context, project, container,
	number string) (core.Task, error) {
	tasks, err := s.tasks(ctx, `SELECT data, version FROM tasks WHERE project = ?
		AND state NOT IN ('completed', 'failed', 'cancelled')`, project)
	if err != nil {
		return core.Task{}, err
	}
	for _, t := range tasks {
		if pr := t.PullRequest; pr != nil && pr.Number == number && pr.Repo.Slug == container {
			return t, nil
		}
	}
	return core.Task{}, core.Errorf(core.KindNotFound, "storage.find_task",
		"no task for pull request %s", number)
}
