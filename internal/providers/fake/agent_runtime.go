package fake

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/ungarscool1/goblins/internal/core"
)

// AgentStep scripts one agent run.
type AgentStep struct {
	Output string
	Events []core.AgentEvent
	Err    error
	// Run, when set, computes the result dynamically and may touch the workspace.
	Run func(ctx context.Context, req core.AgentRequest) (string, error)
}

// Agent is a scripted AgentProvider that consumes no tokens.
type Agent struct {
	Name string
	Caps []core.Capability
	Auth core.AuthStatus

	mu       sync.Mutex
	Steps    []AgentStep
	Requests []core.AgentRequest
	sessions int
	known    map[string]bool
}

// NewAgent returns a resumable, streaming fake agent.
func NewAgent(name string, steps ...AgentStep) *Agent {
	return &Agent{
		Name: name, Steps: steps, Auth: core.AuthStatus{State: core.AuthOK},
		Caps:  []core.Capability{core.CapAgentResume, core.CapAgentStreaming, core.CapAgentEffort},
		known: map[string]bool{},
	}
}

func (a *Agent) Info() core.ProviderInfo { return info(core.ProviderAgent, a.Name, a.Caps) }

// Script appends runs.
func (a *Agent) Script(steps ...AgentStep) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Steps = append(a.Steps, steps...)
}

// RunCount returns how many runs happened.
func (a *Agent) RunCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.Requests)
}

func (a *Agent) Run(ctx context.Context, req core.AgentRequest, sink func(core.AgentEvent)) (core.AgentResult, error) {
	a.mu.Lock()
	a.Requests = append(a.Requests, req)
	session := req.ResumeSessionID
	switch {
	case session != "" && !a.Info().Has(core.CapAgentResume):
		a.mu.Unlock()
		return core.AgentResult{}, core.Unsupported(a.Info(), core.CapAgentResume)
	case session != "" && !a.known[session]:
		a.mu.Unlock()
		return core.AgentResult{}, core.Errorf(core.KindNotFound, "fake.agent", "unknown session %s", session)
	case session == "":
		a.sessions++
		session = fmt.Sprintf("%s-session-%d", a.Name, a.sessions)
		a.known[session] = true
	}
	var step AgentStep
	if len(a.Steps) > 0 {
		step, a.Steps = a.Steps[0], a.Steps[1:]
	}
	a.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return core.AgentResult{}, core.Wrap(core.KindCancelled, "fake.agent", err)
	}
	sink(core.AgentEvent{Kind: core.AgentEventSession, SessionID: session})
	for _, ev := range step.Events {
		sink(ev)
	}
	if step.Err != nil {
		return core.AgentResult{SessionID: session}, step.Err
	}
	out := step.Output
	if step.Run != nil {
		var err error
		if out, err = step.Run(ctx, req); err != nil {
			return core.AgentResult{SessionID: session}, err
		}
	}
	sink(core.AgentEvent{Kind: core.AgentEventResult, SessionID: session, Text: out})
	return core.AgentResult{SessionID: session, Output: out}, nil
}

func (a *Agent) AuthStatus(context.Context, core.ExecutionRuntime) (core.AuthStatus, error) {
	return a.Auth, nil
}

func (a *Agent) Models(context.Context) ([]core.Model, error) {
	return []core.Model{{ID: "fake-small"}, {ID: "fake-large"}}, nil
}

// WorkloadResult scripts how a fake workload ends.
type WorkloadResult struct {
	Exit   int
	Stdout string
	Stderr string
	Hang   bool // stays running until cancelled
}

type workload struct {
	spec   core.WorkloadSpec
	status core.WorkloadStatus
	result WorkloadResult
	done   chan struct{}
}

// Runtime is an in-memory ExecutionRuntime. The type name it reports can be
// overridden to prove that the scheduler does not depend on any backend.
type Runtime struct {
	Name     string
	TypeName string
	Caps     []core.Capability
	// Behavior decides each workload's outcome; nil means instant success.
	Behavior func(core.WorkloadSpec) WorkloadResult

	mu        sync.Mutex
	workloads map[string]*workload
	volumes   map[string]bool
	Created   []core.WorkloadSpec
}

// AllRuntimeCaps lists capabilities of the default fake runtime.
var AllRuntimeCaps = []core.Capability{
	core.CapRuntimeVolumes, core.CapRuntimeNetworkNone, core.CapRuntimeNetworkEgress,
	core.CapRuntimeResourceLimits, core.CapRuntimeReadOnlyRoot,
}

// NewRuntime returns a fake runtime without privileged features.
func NewRuntime(name string) *Runtime {
	return &Runtime{Name: name, TypeName: "fake", Caps: AllRuntimeCaps, workloads: map[string]*workload{}, volumes: map[string]bool{}}
}

func (r *Runtime) Info() core.ProviderInfo {
	i := info(core.ProviderRuntime, r.Name, r.Caps)
	i.Type = r.TypeName
	return i
}

// CheckSpec rejects specs that need capabilities rt does not advertise. It is
// shared with real runtimes so they all refuse the same way.
func CheckSpec(rt core.ExecutionRuntime, spec core.WorkloadSpec) error {
	var req []core.Capability
	if spec.Security.Privileged {
		req = append(req, core.CapRuntimePrivileged)
	}
	if spec.Security.GPU {
		req = append(req, core.CapRuntimeGPU)
	}
	if spec.Security.KVM {
		req = append(req, core.CapRuntimeKVM)
	}
	switch spec.Network {
	case core.NetworkNone:
		req = append(req, core.CapRuntimeNetworkNone)
	case core.NetworkEgress:
		req = append(req, core.CapRuntimeNetworkEgress)
	default:
		return core.Errorf(core.KindInvalid, "runtime.create", "workload %s has no network policy", spec.Name)
	}
	if len(spec.Mounts) > 0 {
		req = append(req, core.CapRuntimeVolumes)
	}
	if spec.Resources != (core.Resources{}) {
		req = append(req, core.CapRuntimeResourceLimits)
	}
	return core.Negotiate(rt, req...)
}

func (r *Runtime) Create(_ context.Context, spec core.WorkloadSpec) (core.WorkloadStatus, error) {
	if spec.Name == "" {
		return core.WorkloadStatus{}, core.Errorf(core.KindInvalid, "fake.runtime", "workload name required")
	}
	if err := CheckSpec(r, spec); err != nil {
		return core.WorkloadStatus{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if w, ok := r.workloads[spec.Name]; ok {
		return w.status, nil
	}
	res := WorkloadResult{}
	if r.Behavior != nil {
		res = r.Behavior(spec)
	}
	w := &workload{spec: spec, result: res, done: make(chan struct{}), status: core.WorkloadStatus{Name: spec.Name, Phase: core.WorkloadRunning, Labels: maps.Clone(spec.Labels)}}
	if !res.Hang {
		w.finish(res.Exit)
	}
	r.workloads[spec.Name] = w
	r.Created = append(r.Created, spec)
	return w.status, nil
}

func (w *workload) finish(exit int) {
	w.status.ExitCode = exit
	w.status.Phase = core.WorkloadSucceeded
	if exit != 0 {
		w.status.Phase = core.WorkloadFailed
	}
	close(w.done)
}

func (r *Runtime) get(name, op string) (*workload, error) {
	if w, ok := r.workloads[name]; ok {
		return w, nil
	}
	return nil, core.Errorf(core.KindNotFound, op, "workload %s not found", name)
}

func (r *Runtime) Status(_ context.Context, name string) (core.WorkloadStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, err := r.get(name, "fake.status")
	if err != nil {
		return core.WorkloadStatus{}, err
	}
	return w.status, nil
}

func (r *Runtime) Logs(ctx context.Context, name string, follow bool, stdout, stderr io.Writer) error {
	r.mu.Lock()
	w, err := r.get(name, "fake.logs")
	r.mu.Unlock()
	if err != nil {
		return err
	}
	if follow {
		select {
		case <-w.done:
		case <-ctx.Done():
			return core.Wrap(core.KindCancelled, "fake.logs", ctx.Err())
		}
	}
	if _, err := io.Copy(stdout, strings.NewReader(w.result.Stdout)); err != nil {
		return err
	}
	_, err = io.Copy(stderr, strings.NewReader(w.result.Stderr))
	return err
}

func (r *Runtime) Cancel(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, err := r.get(name, "fake.cancel")
	if err != nil {
		return err
	}
	if !w.status.Phase.Terminal() {
		w.status.Phase = core.WorkloadCancelled
		w.status.ExitCode = 137
		close(w.done)
	}
	return nil
}

func (r *Runtime) Cleanup(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w, ok := r.workloads[name]; ok && !w.status.Phase.Terminal() {
		w.status.Phase = core.WorkloadCancelled
		close(w.done)
	}
	delete(r.workloads, name)
	return nil
}

func (r *Runtime) List(_ context.Context, labels map[string]string) ([]core.WorkloadStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []core.WorkloadStatus{}
	for _, name := range slices.Sorted(maps.Keys(r.workloads)) {
		w := r.workloads[name]
		match := true
		for k, v := range labels {
			match = match && w.status.Labels[k] == v
		}
		if match {
			out = append(out, w.status)
		}
	}
	return out, nil
}

func (r *Runtime) EnsureVolume(_ context.Context, name string, _ map[string]string) error {
	if !r.Info().Has(core.CapRuntimeVolumes) {
		return core.Unsupported(r.Info(), core.CapRuntimeVolumes)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.volumes[name] = true
	return nil
}

func (r *Runtime) RemoveVolume(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.volumes, name)
	return nil
}

// HasVolume reports whether a volume exists.
func (r *Runtime) HasVolume(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.volumes[name]
}
