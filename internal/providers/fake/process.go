package fake

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// ProcessRuntime runs workloads as local processes, which makes real scripts and
// real Git usable in offline tests.
//
// It is NOT a sandbox. It exists for tests and is never registered as a provider,
// so no configuration can select it.
type ProcessRuntime struct {
	Name string
	// Root holds volumes.
	Root string
	// PathPrefix is prepended to PATH, typically where a fake CLI lives.
	PathPrefix string

	mu        sync.Mutex
	workloads map[string]*procWorkload
	// Specs records every created spec, for asserting what each workload received.
	Specs []core.WorkloadSpec
}

type procWorkload struct {
	status         core.WorkloadStatus
	cmd            *exec.Cmd
	stdout, stderr syncBuffer
	done           chan struct{}
	cancelled      bool
}

// syncBuffer lets Logs read while the process writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) from(off int) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.buf.Bytes()[off:])
}

// NewProcessRuntime returns a process runtime rooted in a directory.
func NewProcessRuntime(name, root, pathPrefix string) *ProcessRuntime {
	return &ProcessRuntime{Name: name, Root: root, PathPrefix: pathPrefix, workloads: map[string]*procWorkload{}}
}

// Info implements core.Provider.
func (r *ProcessRuntime) Info() core.ProviderInfo {
	i := info(core.ProviderRuntime, r.Name, []core.Capability{core.CapRuntimeVolumes, core.CapRuntimeNetworkNone, core.CapRuntimeNetworkEgress, core.CapRuntimeReadOnlyRoot})
	i.Type = "fake-process"
	return i
}

// VolumePath returns where a volume is on disk.
func (r *ProcessRuntime) VolumePath(name string) string {
	return filepath.Join(r.Root, "volumes", name)
}

// Create implements core.ExecutionRuntime. Mount targets in the command, the
// environment and the working directory are rewritten to volume directories.
func (r *ProcessRuntime) Create(_ context.Context, spec core.WorkloadSpec) (core.WorkloadStatus, error) {
	if spec.Name == "" {
		return core.WorkloadStatus{}, core.Errorf(core.KindInvalid, "process.create", "workload name required")
	}
	if err := CheckSpec(r, spec); err != nil {
		return core.WorkloadStatus{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if w, ok := r.workloads[spec.Name]; ok {
		return w.status, nil
	}
	rewrite := func(s string) string {
		for _, m := range spec.Mounts {
			s = strings.ReplaceAll(s, m.Target, r.VolumePath(m.Volume))
		}
		return s
	}
	for _, m := range spec.Mounts {
		if err := os.MkdirAll(r.VolumePath(m.Volume), 0o750); err != nil {
			return core.WorkloadStatus{}, core.Wrap(core.KindInternal, "process.create", err)
		}
	}
	args := make([]string, len(spec.Command))
	for i, a := range spec.Command {
		args[i] = rewrite(a)
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = rewrite(spec.WorkDir)
	if cmd.Dir != "" {
		if err := os.MkdirAll(cmd.Dir, 0o750); err != nil {
			return core.WorkloadStatus{}, core.Wrap(core.KindInternal, "process.create", err)
		}
	}
	// No inherited environment: a workload sees exactly what its spec grants, as in
	// a container. Tests rely on this to prove what a workload does not receive.
	cmd.Env = []string{"PATH=" + r.PathPrefix + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + r.Root}
	for k, v := range spec.Env {
		cmd.Env = append(cmd.Env, k+"="+rewrite(v))
	}
	for k, v := range spec.Secrets {
		cmd.Env = append(cmd.Env, k+"="+v.Reveal())
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	w := &procWorkload{cmd: cmd, done: make(chan struct{}), status: core.WorkloadStatus{Name: spec.Name, Phase: core.WorkloadRunning, Labels: maps.Clone(spec.Labels)}}
	cmd.Stdout, cmd.Stderr = &w.stdout, &w.stderr
	if err := cmd.Start(); err != nil {
		return core.WorkloadStatus{}, core.Wrap(core.KindInvalid, "process.create", err)
	}
	r.workloads[spec.Name] = w
	r.Specs = append(r.Specs, spec)
	go r.wait(w)
	if spec.Timeout > 0 {
		go func() {
			select {
			case <-w.done:
			case <-time.After(spec.Timeout):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		}()
	}
	return w.status, nil
}

func (r *ProcessRuntime) wait(w *procWorkload) {
	err := w.cmd.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	code := 0
	if err != nil {
		code = 1
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() > 0 {
			code = ee.ExitCode()
		}
	}
	w.status.ExitCode = code
	switch {
	case w.cancelled:
		w.status.Phase = core.WorkloadCancelled
	case code == 0:
		w.status.Phase = core.WorkloadSucceeded
	default:
		w.status.Phase = core.WorkloadFailed
	}
	close(w.done)
}

func (r *ProcessRuntime) get(name, op string) (*procWorkload, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w, ok := r.workloads[name]; ok {
		return w, nil
	}
	return nil, core.Errorf(core.KindNotFound, op, "workload %s not found", name)
}

// Status implements core.ExecutionRuntime.
func (r *ProcessRuntime) Status(_ context.Context, name string) (core.WorkloadStatus, error) {
	w, err := r.get(name, "process.status")
	if err != nil {
		return core.WorkloadStatus{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return w.status, nil
}

// Logs implements core.ExecutionRuntime, streaming output as it appears.
func (r *ProcessRuntime) Logs(ctx context.Context, name string, follow bool, stdout, stderr io.Writer) error {
	w, err := r.get(name, "process.logs")
	if err != nil {
		return err
	}
	var outOff, errOff int
	flush := func() error {
		o := w.stdout.from(outOff)
		outOff += len(o)
		if _, err := stdout.Write(o); err != nil {
			return err
		}
		e := w.stderr.from(errOff)
		errOff += len(e)
		_, err := stderr.Write(e)
		return err
	}
	for follow {
		select {
		case <-w.done:
			follow = false
		case <-ctx.Done():
			return core.Wrap(core.KindCancelled, "process.logs", ctx.Err())
		case <-time.After(5 * time.Millisecond):
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// Cancel implements core.ExecutionRuntime.
func (r *ProcessRuntime) Cancel(_ context.Context, name string) error {
	w, err := r.get(name, "process.cancel")
	if err != nil {
		return err
	}
	r.mu.Lock()
	running := !w.status.Phase.Terminal()
	if running {
		w.cancelled = true
	}
	r.mu.Unlock()
	if running {
		_ = syscall.Kill(-w.cmd.Process.Pid, syscall.SIGKILL)
	}
	return nil
}

// Cleanup implements core.ExecutionRuntime.
func (r *ProcessRuntime) Cleanup(ctx context.Context, name string) error {
	w, err := r.get(name, "process.cleanup")
	if core.IsKind(err, core.KindNotFound) {
		return nil
	}
	if err := r.Cancel(ctx, name); err != nil {
		return err
	}
	<-w.done
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.workloads, name)
	return nil
}

// List implements core.ExecutionRuntime.
func (r *ProcessRuntime) List(_ context.Context, labels map[string]string) ([]core.WorkloadStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []core.WorkloadStatus{}
	for _, name := range slices.Sorted(maps.Keys(r.workloads)) {
		st := r.workloads[name].status
		match := true
		for k, v := range labels {
			match = match && st.Labels[k] == v
		}
		if match {
			out = append(out, st)
		}
	}
	return out, nil
}

// EnsureVolume implements core.ExecutionRuntime.
func (r *ProcessRuntime) EnsureVolume(_ context.Context, name string, _ map[string]string) error {
	if err := os.MkdirAll(r.VolumePath(name), 0o750); err != nil {
		return core.Wrap(core.KindInternal, "process.volume", err)
	}
	return nil
}

// RemoveVolume implements core.ExecutionRuntime.
func (r *ProcessRuntime) RemoveVolume(_ context.Context, name string) error {
	if err := os.RemoveAll(r.VolumePath(name)); err != nil {
		return core.Wrap(core.KindInternal, "process.volume", err)
	}
	return nil
}
