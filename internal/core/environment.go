package core

import "context"

// EnvironmentPhase orders an environment's steps. Goblins runs them, in this order, and decides
// what happens after each; a provider only says what the steps are and runs one when asked.
type EnvironmentPhase string

const (
	// PhaseAcquire prepares an environment once per lease, such as provisioning one.
	PhaseAcquire EnvironmentPhase = "acquire"
	// PhaseDeploy puts the tested commit into the environment.
	PhaseDeploy EnvironmentPhase = "deploy"
	// PhaseVerify checks the deployed commit: readiness, smoke and integration checks.
	PhaseVerify EnvironmentPhase = "verify"
	// PhaseEvidence gathers logs, events and diagnostics after a failure.
	PhaseEvidence EnvironmentPhase = "evidence"
	// PhaseRelease resets or destroys the environment when the task is done with it.
	PhaseRelease EnvironmentPhase = "release"
)

// OperationKind says what a step may do to an environment.
type OperationKind string

const (
	OperationRead        OperationKind = "read"
	OperationMutate      OperationKind = "mutate"
	OperationDestructive OperationKind = "destructive"
)

// EnvironmentStep is one operation an operator defined for an environment.
type EnvironmentStep struct {
	Name  string
	Phase EnvironmentPhase
	Kind  OperationKind
	// Idempotent says the step can run again after an interruption. A step that is not, and
	// was interrupted, stops the task for a person rather than run twice.
	Idempotent bool
}

// Environment is one development environment a task uses: a shared target it leased, or one
// provisioned for it alone.
type Environment struct {
	Provider  string
	Target    string
	Ephemeral bool
}

// StepRequest asks a provider to run one step against one environment, for one commit.
type StepRequest struct {
	Env          Environment
	Step         EnvironmentStep
	Holder       string
	SHA          string
	Runtime      ExecutionRuntime
	Workspace    Mount
	WorkDir      string
	WorkloadName string
	Labels       map[string]string
}

// StepResult is what a step did. Output is bounded and has secrets removed.
type StepResult struct {
	OK       bool
	ExitCode int
	TimedOut bool
	Output   string
}

// EnvironmentProvider is a kind of development environment: a dev cluster, a Terraform
// workspace, a project's staging launcher. It knows no workflow: Goblins leases targets, orders
// steps, keeps track of what ran and decides what a failure means.
type EnvironmentProvider interface {
	Provider
	// Targets are the shared environments this provider may use, leased one task at a time.
	// None means each task gets an environment provisioned for it.
	Targets() []string
	Steps() []EnvironmentStep
	RunStep(ctx context.Context, req StepRequest) (StepResult, error)
}
