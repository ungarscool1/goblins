// Package runtime has helpers shared by anything that runs workloads, independent
// of the backend.
package runtime

import (
	"context"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// MaxOutput bounds the output kept by RunToCompletion.
const MaxOutput = 1 << 20

// Result is a finished workload.
type Result struct {
	Status core.WorkloadStatus
	Stdout string
	Stderr string
}

// RunToCompletion starts a workload, waits for it, and always removes it. Cancelling
// ctx cancels the workload.
func RunToCompletion(ctx context.Context, rt core.ExecutionRuntime, spec core.WorkloadSpec) (Result, error) {
	// Set up before creating: an error from Create does not prove nothing started.
	defer func() {
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = rt.Cancel(bg, spec.Name)
		_ = rt.Cleanup(bg, spec.Name)
	}()
	if _, err := rt.Create(ctx, spec); err != nil {
		return Result{}, err
	}
	stdout, stderr := &Tail{Max: MaxOutput}, &Tail{Max: MaxOutput}
	if err := rt.Logs(ctx, spec.Name, true, stdout, stderr); err != nil {
		return Result{}, err
	}
	for {
		st, err := rt.Status(ctx, spec.Name)
		if err != nil {
			return Result{}, err
		}
		if st.Phase.Terminal() {
			return Result{Status: st, Stdout: stdout.String(), Stderr: stderr.String()}, nil
		}
		select {
		case <-ctx.Done():
			return Result{}, core.Wrap(core.KindCancelled, "runtime.run", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Tail keeps the last Max bytes written to it.
type Tail struct {
	Max int
	b   []byte
}

func (t *Tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.Max {
		t.b = t.b[len(t.b)-t.Max:]
	}
	return len(p), nil
}

func (t *Tail) String() string { return string(t.b) }
