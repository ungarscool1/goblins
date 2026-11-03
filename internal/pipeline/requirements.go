package pipeline

import (
	"context"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/scheduler"
)

// acceptedIssue returns the issue with the title and body the workflow last accepted. An
// edit by someone whose input is ignored, such as an author without write access, changes
// the live issue but never what agents read or what a pull request describes.
func acceptedIssue(ctx context.Context, jc scheduler.JobContext, pr *Project) (core.Issue, error) {
	issue, err := pr.Tracker.GetIssue(ctx, issueRef(jc.Task))
	if err != nil {
		return core.Issue{}, err
	}
	if issue.RequirementsHash() == jc.Task.RequirementsHash {
		return issue, nil
	}
	accepted := issue
	accepted.Title, accepted.Body = jc.Task.Title, jc.Task.AcceptedBody
	if accepted.RequirementsHash() != jc.Task.RequirementsHash {
		// Only tasks from before the accepted text was kept get here.
		return core.Issue{}, core.Errorf(core.KindInvalid, "pipeline.issue",
			"the issue text changed after it was accepted, and this task is older than the "+
				"copy Goblins keeps of it, so the accepted text is unknown. Cancel this task "+
				"and open a new issue")
	}
	jc.Log("system", "the issue text changed without being accepted, so the accepted text is used")
	return accepted, nil
}
