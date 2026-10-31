package runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers/fake"
	"github.com/ungarscool1/goblins/internal/runtime"
)

// startedThenFailed starts the workload, then reports failure, as when the engine started
// a container and the status request after it was cut off.
type startedThenFailed struct{ *fake.Runtime }

func (r startedThenFailed) Create(ctx context.Context, spec core.WorkloadSpec) (core.WorkloadStatus, error) {
	if _, err := r.Runtime.Create(ctx, spec); err != nil {
		return core.WorkloadStatus{}, err
	}
	return core.WorkloadStatus{}, errors.New("connection lost")
}

func TestWorkloadIsRemovedWhenCreateFailsAfterStarting(t *testing.T) {
	inner := fake.NewRuntime("r")
	inner.Behavior = func(core.WorkloadSpec) fake.WorkloadResult { return fake.WorkloadResult{Hang: true} }
	spec := core.WorkloadSpec{Name: "w", Image: "i", Command: []string{"x"}, Network: core.NetworkNone}
	if _, err := runtime.RunToCompletion(context.Background(), startedThenFailed{inner}, spec); err == nil {
		t.Fatal("the failure was not reported")
	}
	if _, err := inner.Status(context.Background(), "w"); !core.IsKind(err, core.KindNotFound) {
		t.Fatalf("workload left behind: %v", err)
	}
}
