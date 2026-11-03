package pipeline

import (
	"context"
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers/fake"
	"github.com/ungarscool1/goblins/internal/scheduler"
)

func TestAcceptedIssue(t *testing.T) {
	world := fake.NewWorld()
	author := core.Actor{ID: "2", Login: "author", Type: core.ActorHuman}
	iss := world.AddIssue("o/r", "Add a thing", "Please add a thing", author)
	pr := &Project{Tracker: fake.NewTracker(world, "t")}
	task := core.Task{Subject: core.Subject{Kind: core.SubjectIssue, Container: "o/r",
		ExternalID: iss.Ref.ExternalID}, Title: iss.Title, AcceptedBody: iss.Body,
		RequirementsHash: iss.RequirementsHash()}
	var logged []string
	jc := scheduler.JobContext{Task: task,
		Log: func(_, text string) { logged = append(logged, text) }}

	got, err := acceptedIssue(context.Background(), jc, pr)
	if err != nil || got.Body != "Please add a thing" || len(logged) != 0 {
		t.Fatalf("unchanged issue = %+v, %v, %q", got, err, logged)
	}

	world.EditIssue(iss.Ref, "Something else entirely")
	got, err = acceptedIssue(context.Background(), jc, pr)
	if err != nil || got.Body != "Please add a thing" || got.Title != "Add a thing" ||
		got.URL != iss.URL {
		t.Fatalf("changed issue = %+v, %v", got, err)
	}
	if len(logged) != 1 {
		t.Fatalf("logged %q", logged)
	}

	// A task from before the copy was kept cannot know the accepted text, so it stops.
	jc.Task.AcceptedBody = ""
	if _, err := acceptedIssue(context.Background(), jc, pr); !core.IsKind(err, core.KindInvalid) {
		t.Fatalf("old task: %v", err)
	}
}
