// Package codex runs the Codex CLI under a ChatGPT subscription login. It never reads,
// accepts or falls back to a model API key, and it is optional: a project that does not
// reference it never builds or dispatches it.
//
// Security model (docs/adr/0004): the CLI holds subscription credentials, so it must not
// run commands. Unlike a sandbox, which still lets a command read the login next to it,
// turning the command tools off removes the path altogether. The adapter passes that to
// the CLI, refuses a CLI that cannot do it, and discards any run that executed a command
// anyway.
package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
	"github.com/ungarscool1/goblins/internal/providers/agentcli"
	rt "github.com/ungarscool1/goblins/internal/runtime"
)

const (
	typeName        = "codex-cli"
	homeMount       = "/codex-home"
	maxStderr       = 64 << 10
	preflightWindow = 2 * time.Minute
	loginHint       = "Log in on this installation with a ChatGPT subscription: docker compose --profile codex run --rm -it codex-login"
)

var (
	// requiredFlags must all appear in `codex exec --help`; each one is load-bearing.
	requiredFlags = []string{"--json", "--ignore-user-config", "--ignore-rules", "--disable",
		"--skip-git-repo-check"}
	// requiredFeatures must exist in `codex features list`: they are what running without
	// commands rests on, so a CLI where they have been renamed is not one we can restrict.
	requiredFeatures = []string{"shell_tool", "unified_exec"}
	// harmlessFeatures stay on. Every other feature the CLI reports as enabled is turned off,
	// so a tool that a future version adds is off until someone has looked at it.
	harmlessFeatures = []string{"enable_request_compression", "compaction_image_budget",
		"content_item_kinds", "incremental_tools", "item_ids",
		"unbounded_connection_retries", "system_proxy_fallback", "sqlite"}
	// billingEnv lists variables that could make the CLI bill an API account, or route
	// around the subscription. They are unset in every invocation.
	billingEnv = []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN",
		"OPENAI_BASE_URL", "OPENAI_API_BASE", "OPENAI_ORG_ID", "OPENAI_PROJECT",
		"AZURE_OPENAI_API_KEY"}
)

// Options configures the adapter.
type Options struct {
	Image            string
	Executable       string
	HomeVolume       string
	ResourceDefaults core.Resources
}

// verified is what a preflight learned about one image.
type verified struct {
	disable []string
	models  []core.Model
}

// Agent implements core.AgentProvider.
type Agent struct {
	name string
	opts Options

	mu       sync.Mutex
	verified map[string]verified
}

// New returns an agent.
func New(name string, o Options) *Agent {
	if o.Executable == "" {
		o.Executable = "codex"
	}
	if o.HomeVolume == "" {
		o.HomeVolume = "goblins-codex-home"
	}
	return &Agent{name: name, opts: o, verified: map[string]verified{}}
}

// Register adds the adapter to a registry.
func Register(r *providers.Registry) {
	r.Register(providers.KindAgent, typeName, func(name string, _ providers.Deps,
		opts providers.Options) (any, error) {
		for _, k := range []string{"api_key", "api_key_env", "api_key_file", "access_token",
			"access_token_env", "access_token_file"} {
			if opts.String(k) != "" {
				return nil, core.Errorf(core.KindInvalid, "codex.config",
					"%s is not supported; Goblins only uses a ChatGPT subscription login, so that nothing bills an API account", k)
			}
		}
		if opts.String("image") == "" {
			return nil, core.Errorf(core.KindInvalid, "codex.config", "image is required")
		}
		return New(name, Options{
			Image: opts.String("image"), Executable: opts.String("executable"),
			HomeVolume: opts.String("home_volume"),
			ResourceDefaults: core.Resources{MemoryBytes: int64(opts.Int("memory_mb", 4096)) << 20,
				CPUs: float64(opts.Int("cpus", 2)), PIDs: int64(opts.Int("pids", 512))},
		}), nil
	})
}

// Info implements core.Provider.
func (a *Agent) Info() core.ProviderInfo {
	return core.ProviderInfo{Kind: core.ProviderAgent, Type: typeName, Name: a.name,
		ContractVersion: core.ContractVersion,
		Capabilities: []core.Capability{core.CapAgentResume, core.CapAgentStreaming,
			core.CapAgentEffort}}
}

// Models implements core.AgentProvider. It reports what the catalog of the CLI said at the
// last preflight, and nothing before one, as reading it takes a workload.
func (a *Agent) Models(context.Context) ([]core.Model, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.verified[a.opts.Image].models), nil
}

func (a *Agent) baseSpec(name string, cmd []string) core.WorkloadSpec {
	return core.WorkloadSpec{
		Name: name, Image: a.opts.Image, Network: core.NetworkEgress,
		Command: []string{"sh", "-c", agentcli.Script(billingEnv,
			append([]string{a.opts.Executable}, cmd...))},
		Mounts:    []core.Mount{{Volume: a.opts.HomeVolume, Target: homeMount}},
		Resources: a.opts.ResourceDefaults,
		Security:  core.SecurityProfile{ReadOnlyRoot: true},
		Env:       map[string]string{"CODEX_HOME": homeMount, "HOME": "/tmp"},
	}
}

// args builds the CLI invocation. Everything comes from the abstract policy, so neither
// configuration nor a repository can add a command-running tool. No flag grants anything
// beyond it: the login method is forced, and project instructions are not loaded, because
// a repository's instructions are data like the rest of it.
func args(req core.AgentRequest, disable []string) []string {
	out := []string{"exec"}
	if req.ResumeSessionID != "" {
		out = append(out, "resume")
	}
	sandbox, search := "read-only", "disabled"
	if req.Policy.WriteFiles {
		sandbox = "workspace-write"
	}
	if req.Policy.Web {
		search = "live"
	}
	out = append(out, "--json", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check",
		"-c", `sandbox_mode="`+sandbox+`"`, "-c", `approval_policy="never"`, "-c",
		`web_search="`+search+`"`,
		"-c", `forced_login_method="chatgpt"`, "-c", `cli_auth_credentials_store="file"`, "-c",
		"project_doc_max_bytes=0", "-c", "mcp_servers={}",
		"-c", "check_for_update_on_startup=false", "-c", "history.persistence=\"none\"")
	for _, f := range disable {
		out = append(out, "--disable", f)
	}
	if req.Model != "" {
		out = append(out, "--model", req.Model)
	}
	if req.Effort != "" {
		out = append(out, "-c", `model_reasoning_effort="`+req.Effort+`"`)
	}
	if req.SystemPrompt != "" {
		instructions,
			_ := json.Marshal(req.SystemPrompt) // JSON strings are valid TOML basic strings
		out = append(out, "-c", "developer_instructions="+string(instructions))
	}
	if req.ResumeSessionID != "" {
		out = append(out, req.ResumeSessionID)
	}
	return append(out, "-")
}

// Run implements core.AgentProvider.
func (a *Agent) Run(ctx context.Context, req core.AgentRequest,
	sink func(core.AgentEvent)) (core.AgentResult, error) {
	if req.Runtime == nil {
		return core.AgentResult{}, core.Errorf(core.KindInvalid, "codex.run", "no runtime")
	}
	check, err := a.preflight(ctx, req)
	if err != nil {
		return core.AgentResult{}, err
	}
	if err := checkEffort(check.models, req.Model, req.Effort); err != nil {
		return core.AgentResult{}, err
	}
	// The stream does not say what a run bills to, so this is checked first, on every run.
	st, err := a.authStatus(ctx, req.Runtime, "goblins-auth-"+core.Digest(req.WorkloadName,
		req.IdempotencyKey)[:16], req.Labels)
	if err != nil {
		return core.AgentResult{}, err
	}
	if st.State != core.AuthOK {
		return core.AgentResult{}, core.Errorf(core.KindAuthRequired, "codex.run", "%s", st.Detail)
	}
	name := req.WorkloadName
	if name == "" {
		name = "goblins-agent-" + core.Digest(req.IdempotencyKey)[:16]
	}
	spec := a.baseSpec(name, args(req, check.disable))
	spec.Env["GOBLINS_PROMPT"] = req.Prompt
	spec.Labels, spec.Timeout, spec.WorkDir = req.Labels, req.Timeout, req.WorkDir
	if req.Workspace.Volume != "" {
		w := req.Workspace
		w.ReadOnly = w.ReadOnly || !req.Policy.WriteFiles
		spec.Mounts = append(spec.Mounts, w)
	}
	defer agentcli.Cleanup(ctx, req.Runtime,
		name) // first: an error from Create does not prove nothing started
	if _, err := req.Runtime.Create(ctx, spec); err != nil {
		return core.AgentResult{}, err
	}
	parser := &streamParser{sink: sink}
	stderr := &rt.Tail{Max: maxStderr}
	lines := &agentcli.LineWriter{Fn: parser.line}
	logErr := req.Runtime.Logs(ctx, name, true, lines, stderr)
	lines.Flush()
	if err := ctx.Err(); err != nil {
		return core.AgentResult{SessionID: parser.session}, core.Wrap(core.KindCancelled,
			"codex.run", err)
	}
	if logErr != nil {
		return core.AgentResult{SessionID: parser.session}, core.Wrap(core.KindTransient,
			"codex.logs", logErr)
	}
	done, err := agentcli.WaitDone(ctx, req.Runtime, name)
	if err != nil {
		return core.AgentResult{SessionID: parser.session}, err
	}
	return parser.finish(done, stderr.String())
}

// preflight verifies once per image that the installed CLI can be restricted as the
// security model demands. This fails closed, and uses no model tokens.
func (a *Agent) preflight(ctx context.Context, req core.AgentRequest) (verified, error) {
	a.mu.Lock()
	v, ok := a.verified[a.opts.Image]
	a.mu.Unlock()
	if ok {
		return v, nil
	}
	suffix := core.Digest(req.WorkloadName, req.IdempotencyKey)[:16]
	help, st, err := a.runShort(ctx, req.Runtime, "goblins-preflight-"+suffix, req.Labels,
		[]string{"exec", "--help"})
	if err != nil {
		return v, err
	}
	if st.ExitCode != 0 {
		return v, core.Errorf(core.KindUnsupported, "codex.preflight",
			"the Codex CLI on this image could not be started. Check the agent image")
	}
	var missing []string
	for _, f := range requiredFlags {
		if !strings.Contains(help, f) {
			missing = append(missing, f)
		}
	}
	features, _, err := a.runShort(ctx, req.Runtime, "goblins-features-"+suffix, req.Labels,
		[]string{"features", "list"})
	if err != nil {
		return v, err
	}
	known, enabled := parseFeatures(features)
	for _, f := range requiredFeatures {
		if !known[f] {
			missing = append(missing, "feature "+f)
		}
	}
	if len(missing) > 0 {
		return v, core.Errorf(core.KindUnsupported, "codex.preflight",
			"the installed Codex CLI lacks what Goblins needs to keep it from running commands (%s), so it will not run it. Change the agent image", strings.Join(missing, ", "))
	}
	v.disable = slices.DeleteFunc(enabled,
		func(f string) bool { return slices.Contains(harmlessFeatures, f) })
	// Both always off, even if reported as disabled already: the CLI's defaults can change.
	for _, f := range requiredFeatures {
		if !slices.Contains(v.disable, f) {
			v.disable = append(v.disable, f)
		}
	}
	// The catalog is a convenience: without one, the model is passed on unverified.
	if out, _, err := a.runShort(ctx, req.Runtime, "goblins-models-"+suffix, req.Labels,
		[]string{"debug", "models", "--bundled"}); err == nil {
		v.models = parseCatalog(out)
	}
	a.mu.Lock()
	a.verified[a.opts.Image] = v
	a.mu.Unlock()
	return v, nil
}

// parseFeatures reads `name stage enabled` lines; anything else is ignored.
func parseFeatures(out string) (known map[string]bool, enabled []string) {
	known = map[string]bool{}
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		known[f[0]] = true
		if f[len(f)-1] == "true" {
			enabled = append(enabled, f[0])
		}
	}
	slices.Sort(enabled)
	return known, enabled
}

func parseCatalog(out string) []core.Model {
	var cat struct {
		Models []struct {
			Slug   string `json:"slug"`
			Name   string `json:"display_name"`
			Levels []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	i, j := strings.IndexByte(out, '{'), strings.LastIndexByte(out, '}')
	if i < 0 || j < i || json.Unmarshal([]byte(out[i:j+1]), &cat) != nil {
		return nil
	}
	models := make([]core.Model, 0, len(cat.Models))
	for _, m := range cat.Models {
		cm := core.Model{ID: m.Slug, Description: m.Name}
		for _, l := range m.Levels {
			cm.Efforts = append(cm.Efforts, l.Effort)
		}
		models = append(models, cm)
	}
	return models
}

// checkEffort refuses an effort the catalog says the model cannot do, which would otherwise
// surface as an opaque failure after spending a run. Unknown models pass: catalogs lag.
func checkEffort(models []core.Model, model, effort string) error {
	if effort == "" || model == "" {
		return nil
	}
	for _, m := range models {
		if m.ID == model && len(m.Efforts) > 0 && !slices.Contains(m.Efforts, effort) {
			return core.Errorf(core.KindInvalid, "codex.run",
				"model %s does not support effort %q (it supports: %s); change the stage configuration", model, effort, strings.Join(m.Efforts, ", "))
		}
	}
	return nil
}

func (a *Agent) runShort(ctx context.Context, runtime core.ExecutionRuntime, name string,
	labels map[string]string, cliArgs []string) (string, core.WorkloadStatus, error) {
	spec := a.baseSpec(name, cliArgs)
	spec.Timeout = preflightWindow
	spec.Labels = labels // so that a crash here leaves nothing reconciliation cannot find
	defer agentcli.Cleanup(ctx, runtime,
		name) // first: an error from Create does not prove nothing started
	if _, err := runtime.Create(ctx, spec); err != nil {
		return "", core.WorkloadStatus{}, err
	}
	out, errb := &rt.Tail{Max: 8 * maxStderr}, &rt.Tail{Max: maxStderr}
	if err := runtime.Logs(ctx, name, true, out, errb); err != nil {
		return "", core.WorkloadStatus{}, core.Wrap(core.KindTransient, "codex.short", err)
	}
	st, err := agentcli.WaitDone(ctx, runtime, name)
	if err != nil {
		return "", st, err
	}
	return out.String() + errb.String(), st, nil
}

// AuthStatus implements core.AgentProvider. It runs no model call, so it can back health
// checks without spending anything. A login that is not a subscription is reported as
// unusable, because using it would bill an API account.
func (a *Agent) AuthStatus(ctx context.Context, runtime core.ExecutionRuntime) (core.AuthStatus,
	error) {
	st, err := a.authStatus(ctx, runtime, fmt.Sprintf("goblins-auth-%s-%d", a.name,
		time.Now().UnixNano()), nil)
	if err != nil {
		return core.AuthStatus{State: core.AuthUnknown, Detail: core.UserMessage(err)}, err
	}
	return st, nil
}

var (
	apiLogin          = regexp.MustCompile(`(?i)api[ _-]?key|access[ _-]?token`)
	subscriptionLogin = regexp.MustCompile(`(?i)chatgpt`)
)

func (a *Agent) authStatus(ctx context.Context, runtime core.ExecutionRuntime, name string,
	labels map[string]string) (core.AuthStatus, error) {
	out, st, err := a.runShort(ctx, runtime, name, labels, []string{"login", "status"})
	if err != nil {
		return core.AuthStatus{}, err
	}
	switch {
	case apiLogin.MatchString(out):
		return core.AuthStatus{State: core.AuthLoginRequired,
			Detail: "the Codex CLI is not logged in with a ChatGPT subscription, and Goblins never uses API billing. " + loginHint}, nil
	case st.ExitCode != 0 || !subscriptionLogin.MatchString(out):
		return core.AuthStatus{State: core.AuthLoginRequired, Detail: loginHint}, nil
	}
	return core.AuthStatus{State: core.AuthOK, Detail: "chatgpt"}, nil
}

// streamParser turns `codex exec --json` lines into events and a final result.
type streamParser struct {
	sink      func(core.AgentEvent)
	session   string
	announced bool
	output    string
	completed bool
	failure   string
	command   string
}

type streamLine struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
	Error    struct {
		Message string `json:"message"`
	} `json:"error"`
	Item struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
}

func (p *streamParser) line(raw []byte) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return
	}
	var l streamLine
	if json.Unmarshal(raw, &l) != nil {
		return
	}
	switch l.Type {
	case "thread.started":
		if l.ThreadID != "" {
			p.session = l.ThreadID
			if !p.announced {
				p.announced = true
				p.sink(core.AgentEvent{Kind: core.AgentEventSession, SessionID: l.ThreadID})
			}
		}
	case "item.started", "item.completed":
		switch l.Item.Type {
		case "command_execution", "mcp_tool_call", "collab_tool_call":
			p.command = l.Item.Type
		case "agent_message":
			if l.Type == "item.completed" {
				p.output = l.Item.Text
				p.sink(core.AgentEvent{Kind: core.AgentEventText, SessionID: p.session,
					Text: l.Item.Text})
			}
			return
		case "reasoning", "todo_list", "error":
			return
		}
		if l.Type == "item.started" && l.Item.Type != "" {
			p.sink(core.AgentEvent{Kind: core.AgentEventTool, SessionID: p.session,
				Text: l.Item.Type})
		}
	case "turn.completed":
		p.completed = true
	case "turn.failed":
		p.failure += " " + l.Error.Message
	case "error":
		p.failure += " " + l.Message
	}
}

func (p *streamParser) finish(st core.WorkloadStatus, stderr string) (core.AgentResult, error) {
	res := core.AgentResult{SessionID: p.session}
	if p.command != "" {
		return res, core.Errorf(core.KindUnsupported, "codex.run",
			"the Codex CLI used a %s tool despite being told not to, so the result was discarded. Goblins will not run this CLI next to its credentials; change the agent image", p.command)
	}
	if p.completed && p.failure == "" && st.ExitCode == 0 {
		res.Output = p.output
		p.sink(core.AgentEvent{Kind: core.AgentEventResult, SessionID: p.session, Text: res.Output})
		return res, nil
	}
	return res, classify(p.failure+" "+stderr, st)
}

var (
	quotaPattern  = regexp.MustCompile(`(?i)usage limit|rate[ _-]?limit|limit reached|hit your .*limit|quota|too many requests|\b429\b`)
	authPattern   = regexp.MustCompile(`(?i)not logged in|please (run )?(codex )?login|log ?in again|unauthorized|\b401\b|\b403\b|token[ _](has )?expired|refresh token|authentication`)
	resumePattern = regexp.MustCompile(`(?i)no (rollout|session|thread|conversation)[^.]* found|(session|thread|conversation|rollout) [^.]*not found`)
)

// classify maps a failure onto workflow-relevant kinds. Messages are fixed strings rather
// than CLI output, which could contain anything.
func classify(text string, st core.WorkloadStatus) error {
	switch {
	case authPattern.MatchString(text):
		return core.Errorf(core.KindAuthRequired, "codex.run",
			"the ChatGPT subscription login is missing or expired. %s", loginHint)
	case quotaPattern.MatchString(text):
		return core.Errorf(core.KindQuotaExhausted, "codex.run",
			"the ChatGPT subscription usage limit was reached")
	case resumePattern.MatchString(text):
		return core.Errorf(core.KindNotFound, "codex.run",
			"the agent session can no longer be resumed")
	case st.Phase == core.WorkloadTimedOut:
		return core.Errorf(core.KindTimeout, "codex.run", "the agent run exceeded its time limit")
	case st.Phase == core.WorkloadCancelled:
		return core.Errorf(core.KindCancelled, "codex.run", "the agent run was cancelled")
	}
	return core.Errorf(core.KindTransient, "codex.run", "the agent exited unexpectedly (exit %d)",
		st.ExitCode)
}
