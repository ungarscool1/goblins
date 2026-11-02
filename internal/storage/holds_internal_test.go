package storage

import (
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
)

func TestClaimOrderIsFairAcrossProjects(t *testing.T) {
	jobs := []core.Job{
		{ID: "busy-1", Project: "busy", LimitKey: "acct"},
		{ID: "busy-2", Project: "busy", LimitKey: "acct"},
		{ID: "quiet", Project: "quiet", LimitKey: "acct"},
		{ID: "urgent", Project: "busy", LimitKey: "acct", Priority: 10},
		{ID: "held", Project: "quiet", LimitKey: "gone"},
		{ID: "low", Project: "quiet", Priority: -1},
	}
	running := runningCounts{byProject: map[string]int{"busy": 2}, byLimit: map[string]int{}}
	got := claimOrder(jobs, running, core.JobClaim{Held: map[string]bool{"gone": true}})
	want := []string{"urgent", "quiet", "busy-1", "busy-2"}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i, j := range got {
		if j.ID != want[i] {
			t.Fatalf("position %d: got %s, want %s", i, j.ID, want[i])
		}
	}
}
