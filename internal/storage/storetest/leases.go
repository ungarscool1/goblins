package storetest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// Leases runs the lease part of the conformance suite.
func Leases(t *testing.T, build func(t *testing.T) core.StateStore) {
	ctx := context.Background()
	t0 := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	lease := func(holder string, until time.Time, data map[string]string) core.Lease {
		return core.Lease{Resource: "env:dev:dev-1", Holder: holder, AcquiredAt: t0,
			ExpiresAt: until, Data: data}
	}
	t.Run("one holder at a time", func(t *testing.T) {
		s := build(t)
		got, err := s.AcquireLease(ctx, lease("task-a", t0.Add(time.Hour),
			map[string]string{"acquired": "yes"}), t0)
		if err != nil || got.Holder != "task-a" || got.Data["acquired"] != "yes" {
			t.Fatalf("first = %+v, %v", got, err)
		}
		got, err = s.AcquireLease(ctx, lease("task-b", t0.Add(time.Hour), nil), t0.Add(time.Minute))
		if !core.IsKind(err, core.KindConflict) || got.Holder != "task-a" {
			t.Fatalf("second holder = %+v, %v", got, err)
		}
		// The holder renewing keeps its notes.
		got, err = s.AcquireLease(ctx, lease("task-a", t0.Add(2*time.Hour), nil),
			t0.Add(time.Minute))
		if err != nil || got.Data["acquired"] != "yes" ||
			!got.ExpiresAt.Equal(t0.Add(2*time.Hour)) {
			t.Fatalf("renewal = %+v, %v", got, err)
		}
		// Once expired, another holder takes it over, without the old notes.
		got, err = s.AcquireLease(ctx, lease("task-b", t0.Add(4*time.Hour), nil),
			t0.Add(3*time.Hour))
		if err != nil || got.Holder != "task-b" || got.Data["acquired"] != "" {
			t.Fatalf("takeover = %+v, %v", got, err)
		}
	})
	t.Run("update, list and release", func(t *testing.T) {
		s := build(t)
		if _, err := s.AcquireLease(ctx, lease("task-a", t0.Add(time.Hour), nil), t0); err != nil {
			t.Fatal(err)
		}
		other := lease("task-a", t0.Add(time.Hour), nil)
		other.Resource = "other:x"
		if _, err := s.AcquireLease(ctx, other, t0); err != nil {
			t.Fatal(err)
		}
		up := lease("task-a", t0.Add(5*time.Hour), map[string]string{"broken": "reset failed"})
		if err := s.UpdateLease(ctx, up); err != nil {
			t.Fatal(err)
		}
		up.Holder = "task-b"
		if err := s.UpdateLease(ctx, up); !core.IsKind(err, core.KindConflict) {
			t.Fatalf("update by another holder: %v", err)
		}
		list, err := s.ListLeases(ctx, "env:")
		if err != nil || len(list) != 1 || list[0].Data["broken"] != "reset failed" {
			t.Fatalf("list = %+v, %v", list, err)
		}
		if err := s.ReleaseLease(ctx, "env:dev:dev-1", "task-b"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetLease(ctx, "env:dev:dev-1"); err != nil {
			t.Fatal("another holder released the lease")
		}
		if err := s.ReleaseLease(ctx, "env:dev:dev-1", "task-a"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetLease(ctx, "env:dev:dev-1"); !core.IsKind(err, core.KindNotFound) {
			t.Fatalf("after release: %v", err)
		}
	})
	t.Run("racing holders", func(t *testing.T) {
		s := build(t)
		var won atomic.Int32
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				h := lease(string(rune('a'+i)), t0.Add(time.Hour), nil)
				if _, err := s.AcquireLease(ctx, h, t0); err == nil {
					won.Add(1)
				}
			}()
		}
		wg.Wait()
		if won.Load() != 1 {
			t.Fatalf("%d holders won", won.Load())
		}
	})
}
