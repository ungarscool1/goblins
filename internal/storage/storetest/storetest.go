// Package storetest is the conformance suite for core.StateStore.
package storetest

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func subject(id string) core.Subject {
	return core.Subject{Kind: core.SubjectIssue, Provider: "p", Container: "c", ExternalID: id}
}

func event(key, subj string) core.Event {
	return core.Event{Project: "proj", Source: "s", Type: core.EventIssueOpened, Subject: subject(subj), DedupKey: key, OccurredAt: t0, ReceivedAt: t0}
}

// addTask creates a task by consuming an event, the only way tasks are created.
func addTask(t *testing.T, s core.StateStore, id string, jobs ...core.JobDraft) core.Task {
	t.Helper()
	ctx := context.Background()
	if _, _, err := s.AppendEvent(ctx, event("seed-"+id, id)); err != nil {
		t.Fatal(err)
	}
	ev, err := s.ClaimEvent(ctx, "seed", time.Minute, t0)
	if err != nil || ev == nil {
		t.Fatalf("claim seed: %v %v", ev, err)
	}
	task := core.Task{ID: id, Project: "proj", Kind: core.TaskIssue, Subject: subject(id), State: core.StateInvestigating}
	if err := s.Commit(ctx, core.StateCommit{EventSeq: ev.Seq, EventOwner: "seed", Task: &task, Jobs: jobs, Now: t0}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// Run executes the suite. build must return a fresh, empty store.
func Run(t *testing.T, build func(t *testing.T) core.StateStore) {
	ctx := context.Background()

	t.Run("event dedup", func(t *testing.T) {
		s := build(t)
		a, inserted, err := s.AppendEvent(ctx, event("k", "1"))
		if err != nil || !inserted || a.Seq == 0 {
			t.Fatalf("first append: %+v %v %v", a, inserted, err)
		}
		dup := event("k", "1")
		dup.DeliveryID = "another-delivery"
		b, inserted, err := s.AppendEvent(ctx, dup)
		if err != nil || inserted || b.Seq != a.Seq {
			t.Fatalf("duplicate: %+v %v %v", b, inserted, err)
		}
		if b.DeliveryID != "" {
			t.Fatal("stored event was overwritten: envelopes are immutable")
		}
		if _, _, err := s.AppendEvent(ctx, core.Event{Project: "p"}); !core.IsKind(err, core.KindInvalid) {
			t.Fatalf("no dedup key: %v", err)
		}
	})

	t.Run("events serialize per subject", func(t *testing.T) {
		s := build(t)
		for _, e := range []core.Event{event("a1", "a"), event("a2", "a"), event("b1", "b")} {
			if _, _, err := s.AppendEvent(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
		first, err := s.ClaimEvent(ctx, "w1", time.Minute, t0)
		if err != nil || first == nil || first.DedupKey != "a1" {
			t.Fatalf("first = %+v, %v", first, err)
		}
		second, err := s.ClaimEvent(ctx, "w2", time.Minute, t0)
		if err != nil || second == nil || second.DedupKey != "b1" {
			t.Fatalf("a2 must wait for a1, got %+v, %v", second, err)
		}
		none, err := s.ClaimEvent(ctx, "w3", time.Minute, t0)
		if err != nil || none != nil {
			t.Fatalf("nothing should be claimable: %+v, %v", none, err)
		}
		// An expired lease is claimable by another worker, and the old one is fenced.
		again, err := s.ClaimEvent(ctx, "w4", time.Minute, t0.Add(2*time.Minute))
		if err != nil || again == nil || again.DedupKey != "a1" {
			t.Fatalf("expired = %+v, %v", again, err)
		}
		err = s.Commit(ctx, core.StateCommit{EventSeq: first.Seq, EventOwner: "w1", Now: t0})
		if !core.IsKind(err, core.KindConflict) {
			t.Fatalf("fenced commit: %v", err)
		}
		if err := s.Commit(ctx, core.StateCommit{EventSeq: first.Seq, EventOwner: "w4", Now: t0}); err != nil {
			t.Fatal(err)
		}
		next, err := s.ClaimEvent(ctx, "w5", time.Minute, t0.Add(10*time.Minute))
		if err != nil || next == nil {
			t.Fatalf("next = %+v, %v", next, err)
		}
	})

	t.Run("release delays retry", func(t *testing.T) {
		s := build(t)
		if _, _, err := s.AppendEvent(ctx, event("r", "r")); err != nil {
			t.Fatal(err)
		}
		ev, _ := s.ClaimEvent(ctx, "w", time.Minute, t0)
		if err := s.ReleaseEvent(ctx, ev.Seq, "w", t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.ClaimEvent(ctx, "w", time.Minute, t0.Add(time.Minute)); got != nil {
			t.Fatal("claimed before not_before")
		}
		if got, _ := s.ClaimEvent(ctx, "w", time.Minute, t0.Add(2*time.Hour)); got == nil {
			t.Fatal("not claimable after not_before")
		}
	})

	t.Run("commit is atomic and versioned", func(t *testing.T) {
		s := build(t)
		task := addTask(t, s, "t1")
		if task.Version != 1 {
			t.Fatalf("version = %d", task.Version)
		}
		if _, _, err := s.AppendEvent(ctx, event("e2", "t1")); err != nil {
			t.Fatal(err)
		}
		ev, _ := s.ClaimEvent(ctx, "w", time.Minute, t0)
		next := task
		next.State = core.StateWaitingForUser
		// A stale version changes nothing, including the event and the jobs.
		err := s.Commit(ctx, core.StateCommit{EventSeq: ev.Seq, EventOwner: "w", Task: &next, ExpectedVersion: 99, Now: t0,
			Jobs:       []core.JobDraft{{Kind: "x", IdempotencyKey: "stale"}},
			Design:     &core.Design{Revision: 1, Hash: "h", Body: "b"},
			Transition: &core.Transition{From: task.State, To: next.State, Reason: "r"},
		})
		if !core.IsKind(err, core.KindConflict) {
			t.Fatalf("stale version: %v", err)
		}
		if jobs, _ := s.ListJobs(ctx, "t1"); len(jobs) != 0 {
			t.Fatal("job leaked from failed commit")
		}
		if trs, _ := s.ListTransitions(ctx, "t1"); len(trs) != 0 {
			t.Fatal("transition leaked from failed commit")
		}
		err = s.Commit(ctx, core.StateCommit{EventSeq: ev.Seq, EventOwner: "w", Task: &next, ExpectedVersion: 1, Now: t0.Add(time.Second),
			Jobs:       []core.JobDraft{{Kind: "x", IdempotencyKey: "good"}, {Kind: "x", IdempotencyKey: "good"}},
			Design:     &core.Design{Revision: 1, Hash: "h", Body: "b"},
			Transition: &core.Transition{From: task.State, To: next.State, Reason: "r"},
		})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetTask(ctx, "t1")
		if got.State != core.StateWaitingForUser || got.Version != 2 {
			t.Fatalf("task = %+v", got)
		}
		if jobs, _ := s.ListJobs(ctx, "t1"); len(jobs) != 1 {
			t.Fatalf("jobs = %d, idempotency key must deduplicate", len(jobs))
		}
		trs, _ := s.ListTransitions(ctx, "t1")
		if len(trs) != 1 || trs[0].To != core.StateWaitingForUser || trs[0].EventSeq != ev.Seq {
			t.Fatalf("transitions = %+v", trs)
		}
		d, err := s.GetDesign(ctx, "t1", 1)
		if err != nil || d.Hash != "h" || d.Body != "b" {
			t.Fatalf("design = %+v, %v", d, err)
		}
		if _, err := s.GetDesign(ctx, "t1", 2); !core.IsKind(err, core.KindNotFound) {
			t.Fatalf("missing design: %v", err)
		}
		if _, err := s.GetTask(ctx, "nope"); !core.IsKind(err, core.KindNotFound) {
			t.Fatalf("missing task: %v", err)
		}
		if found, err := s.FindTask(ctx, "proj", subject("t1")); err != nil || found.ID != "t1" {
			t.Fatalf("find = %+v, %v", found, err)
		}
		if list, _ := s.ListTasks(ctx, "proj"); len(list) != 1 {
			t.Fatalf("list = %d", len(list))
		}
	})

	t.Run("job lease fencing and retry", func(t *testing.T) {
		s := build(t)
		addTask(t, s, "t", core.JobDraft{Kind: "work", IdempotencyKey: "j1", Payload: json.RawMessage(`{"a":1}`), MaxAttempts: 2})
		claim := core.JobClaim{Owner: "w1", LeaseFor: time.Minute, Now: t0}
		j, err := s.ClaimJob(ctx, claim)
		if err != nil || j == nil || j.Attempt != 1 || string(j.Payload) != `{"a":1}` {
			t.Fatalf("claim = %+v, %v", j, err)
		}
		if again, _ := s.ClaimJob(ctx, core.JobClaim{Owner: "w2", LeaseFor: time.Minute, Now: t0}); again != nil {
			t.Fatal("double claim")
		}
		if err := s.HeartbeatJob(ctx, j.ID, "w1", 1, t0.Add(5*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if again, _ := s.ClaimJob(ctx, core.JobClaim{Owner: "w2", LeaseFor: time.Minute, Now: t0.Add(2 * time.Minute)}); again != nil {
			t.Fatal("heartbeat did not extend the lease")
		}
		if err := s.SaveCheckpoint(ctx, j.ID, "w1", 1, json.RawMessage(`{"session":"s"}`)); err != nil {
			t.Fatal(err)
		}
		// The lease expires: another worker takes over and the first is fenced.
		late := t0.Add(6 * time.Minute)
		j2, err := s.ClaimJob(ctx, core.JobClaim{Owner: "w2", LeaseFor: time.Minute, Now: late})
		if err != nil || j2 == nil || j2.Attempt != 2 || string(j2.Checkpoint) != `{"session":"s"}` {
			t.Fatalf("takeover = %+v, %v", j2, err)
		}
		for name, err := range map[string]error{
			"heartbeat":  s.HeartbeatJob(ctx, j.ID, "w1", 1, late.Add(time.Hour)),
			"checkpoint": s.SaveCheckpoint(ctx, j.ID, "w1", 1, nil),
			"retry":      s.RetryJob(ctx, j.ID, "w1", 1, late, "x"),
			"finish":     s.FinishJob(ctx, core.JobFinish{JobID: j.ID, Owner: "w1", Attempt: 1, State: core.JobSucceeded, Now: late}),
		} {
			if !core.IsKind(err, core.KindConflict) {
				t.Fatalf("fenced %s: %v", name, err)
			}
		}
		if err := s.RetryJob(ctx, j.ID, "w2", 2, late.Add(time.Hour), "boom"); err != nil {
			t.Fatal(err)
		}
		if soon, _ := s.ClaimJob(ctx, core.JobClaim{Owner: "w3", LeaseFor: time.Minute, Now: late}); soon != nil {
			t.Fatal("retry ignored not_before")
		}
		ev := event("done", "t")
		ev.Type = core.EventJobFinished
		j3, _ := s.ClaimJob(ctx, core.JobClaim{Owner: "w3", LeaseFor: time.Minute, Now: late.Add(2 * time.Hour)})
		if j3 == nil || j3.LastError != "boom" {
			t.Fatalf("retried = %+v", j3)
		}
		if err := s.FinishJob(ctx, core.JobFinish{JobID: j.ID, Owner: "w3", Attempt: j3.Attempt, State: core.JobSucceeded, Event: &ev, Now: late}); err != nil {
			t.Fatal(err)
		}
		// The outcome event is durable with the job's end.
		if _, inserted, _ := s.AppendEvent(ctx, ev); inserted {
			t.Fatal("outcome event was not stored by FinishJob")
		}
		got, _ := s.GetJob(ctx, j.ID)
		if got.State != core.JobSucceeded {
			t.Fatalf("state = %s", got.State)
		}
	})

	t.Run("exhausted job fails with an outcome event", func(t *testing.T) {
		s := build(t)
		addTask(t, s, "t", core.JobDraft{Kind: "work", IdempotencyKey: "j", MaxAttempts: 1})
		j, _ := s.ClaimJob(ctx, core.JobClaim{Owner: "w1", LeaseFor: time.Minute, Now: t0})
		if j == nil {
			t.Fatal("no job")
		}
		late := t0.Add(time.Hour)
		if again, err := s.ClaimJob(ctx, core.JobClaim{Owner: "w2", LeaseFor: time.Minute, Now: late}); err != nil || again != nil {
			t.Fatalf("poison job handed out again: %+v %v", again, err)
		}
		got, _ := s.GetJob(ctx, j.ID)
		if got.State != core.JobFailed {
			t.Fatalf("state = %s", got.State)
		}
		ev, err := s.ClaimEvent(ctx, "w", time.Minute, late)
		if err != nil || ev == nil || ev.Job == nil || ev.Job.ErrorKind != core.KindTimeout || ev.Job.Key != "j" {
			t.Fatalf("outcome = %+v, %v", ev, err)
		}
	})

	t.Run("cancelled job stops its worker", func(t *testing.T) {
		s := build(t)
		task := addTask(t, s, "t", core.JobDraft{Kind: "work", IdempotencyKey: "j"})
		j, _ := s.ClaimJob(ctx, core.JobClaim{Owner: "w", LeaseFor: time.Minute, Now: t0})
		if _, _, err := s.AppendEvent(ctx, event("cancel", "t")); err != nil {
			t.Fatal(err)
		}
		ev, _ := s.ClaimEvent(ctx, "e", time.Minute, t0)
		task.State = core.StateCancelled
		if err := s.Commit(ctx, core.StateCommit{EventSeq: ev.Seq, EventOwner: "e", Task: &task, ExpectedVersion: task.Version, CancelKinds: []core.JobKind{"work"}, Now: t0}); err != nil {
			t.Fatal(err)
		}
		if err := s.HeartbeatJob(ctx, j.ID, "w", 1, t0.Add(time.Hour)); !core.IsKind(err, core.KindConflict) {
			t.Fatalf("heartbeat after cancel: %v", err)
		}
		if err := s.FinishJob(ctx, core.JobFinish{JobID: j.ID, Owner: "w", Attempt: 1, State: core.JobSucceeded, Now: t0}); !core.IsKind(err, core.KindConflict) {
			t.Fatalf("finish after cancel: %v", err)
		}
	})

	t.Run("concurrency limits", func(t *testing.T) {
		s := build(t)
		addTask(t, s, "t",
			core.JobDraft{Kind: "agent", IdempotencyKey: "a", LimitKey: "acct"},
			core.JobDraft{Kind: "agent", IdempotencyKey: "b", LimitKey: "acct"},
			core.JobDraft{Kind: "agent", IdempotencyKey: "c", LimitKey: "other"},
			core.JobDraft{Kind: "comment", IdempotencyKey: "light"},
		)
		claim := core.JobClaim{Owner: "w", LeaseFor: time.Hour, Now: t0, MaxPerLimit: map[string]int{"acct": 1, "other": 1}, MaxGlobal: 10}
		seen := map[string]bool{}
		for range 4 {
			j, err := s.ClaimJob(ctx, claim)
			if err != nil {
				t.Fatal(err)
			}
			if j != nil {
				seen[j.IdempotencyKey] = true
			}
		}
		if seen["a"] == seen["b"] || !seen["c"] || !seen["light"] {
			t.Fatalf("limits not applied: %v", seen)
		}
		global := build(t)
		addTask(t, global, "g", core.JobDraft{Kind: "agent", IdempotencyKey: "x", LimitKey: "k"}, core.JobDraft{Kind: "agent", IdempotencyKey: "y", LimitKey: "k"}, core.JobDraft{Kind: "comment", IdempotencyKey: "z"})
		claim = core.JobClaim{Owner: "w", LeaseFor: time.Hour, Now: t0, MaxPerProject: 1}
		n := 0
		for range 3 {
			if j, _ := global.ClaimJob(ctx, claim); j != nil {
				n++
			}
		}
		if n != 2 {
			t.Fatalf("claimed %d, want one heavy and one light", n)
		}
	})

	t.Run("priority wins", func(t *testing.T) {
		s := build(t)
		addTask(t, s, "t", core.JobDraft{Kind: "w", IdempotencyKey: "low"}, core.JobDraft{Kind: "w", IdempotencyKey: "high", Priority: 10})
		j, _ := s.ClaimJob(ctx, core.JobClaim{Owner: "w", LeaseFor: time.Minute, Now: t0})
		if j == nil || j.IdempotencyKey != "high" {
			t.Fatalf("job = %+v", j)
		}
	})

	t.Run("concurrent workers claim each job once", func(t *testing.T) {
		s := build(t)
		var drafts []core.JobDraft
		for i := range 20 {
			drafts = append(drafts, core.JobDraft{Kind: "w", IdempotencyKey: fmt.Sprintf("j%d", i)})
		}
		addTask(t, s, "t", drafts...)
		var wg sync.WaitGroup
		var claimed atomic.Int64
		var mu sync.Mutex
		seen := map[string]int{}
		for w := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 40 {
					j, err := s.ClaimJob(ctx, core.JobClaim{Owner: fmt.Sprintf("w%d", w), LeaseFor: time.Hour, Now: t0})
					if err != nil {
						t.Errorf("claim: %v", err)
						return
					}
					if j == nil {
						continue
					}
					claimed.Add(1)
					mu.Lock()
					seen[j.ID]++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if claimed.Load() != 20 {
			t.Fatalf("claimed %d of 20", claimed.Load())
		}
		for id, n := range seen {
			if n != 1 {
				t.Fatalf("job %s claimed %d times", id, n)
			}
		}
	})

	t.Run("due tasks", func(t *testing.T) {
		s := build(t)
		task := addTask(t, s, "t")
		if _, _, err := s.AppendEvent(ctx, event("p", "t")); err != nil {
			t.Fatal(err)
		}
		ev, _ := s.ClaimEvent(ctx, "e", time.Minute, t0)
		task.PausedUntil = t0.Add(time.Hour)
		if err := s.Commit(ctx, core.StateCommit{EventSeq: ev.Seq, EventOwner: "e", Task: &task, ExpectedVersion: 1, Now: t0}); err != nil {
			t.Fatal(err)
		}
		if due, _ := s.DueTasks(ctx, t0); len(due) != 0 {
			t.Fatal("due too early")
		}
		if due, _ := s.DueTasks(ctx, t0.Add(2*time.Hour)); len(due) != 1 {
			t.Fatal("not due")
		}
	})

	t.Run("cursors and transcript", func(t *testing.T) {
		s := build(t)
		if v, err := s.GetCursor(ctx, "c"); err != nil || v != "" {
			t.Fatalf("empty cursor: %q %v", v, err)
		}
		for _, v := range []string{"1", "2"} {
			if err := s.SetCursor(ctx, "c", v); err != nil {
				t.Fatal(err)
			}
		}
		if v, _ := s.GetCursor(ctx, "c"); v != "2" {
			t.Fatalf("cursor = %q", v)
		}
		entries := []core.TranscriptEntry{{TaskID: "t", JobID: "j", Kind: "text", Text: "one", At: t0}, {TaskID: "t", JobID: "j", Kind: "text", Text: "two", At: t0}, {TaskID: "t", JobID: "j", Kind: "text", Text: "three", At: t0}}
		if err := s.AppendTranscript(ctx, entries); err != nil {
			t.Fatal(err)
		}
		got, err := s.ListTranscript(ctx, "t", 2)
		if err != nil || len(got) != 2 || got[0].Text != "two" || got[1].Text != "three" {
			t.Fatalf("transcript = %+v, %v", got, err)
		}
		if err := s.Ping(ctx); err != nil {
			t.Fatal(err)
		}
	})
}
