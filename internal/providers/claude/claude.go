// Package claude runs the Claude Code CLI under a subscription login. It never
// reads, accepts or falls back to a model API key.
//
// Security model (docs/adr/0004): the CLI holds subscription credentials, so it is
// started in restricted mode, with no command-running tools and with file tools
// confined to the workspace. The adapter refuses to run a CLI that cannot do this.
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
)

const (
	typeName        = "claude-cli"
	configMount     = "/claude-config"
	maxLine         = 32 << 20
	maxStderr       = 64 << 10
	preflightWindow = 2 * time.Minute
)

// requiredFlags must all appear in `claude --help`; each one is load-bearing for
// the security model or for the protocol.
var requiredFlags = []string{"--restricted", "--strict-mcp-config", "--tools", "--allowedTools", "--permission-mode", "--output-format", "--resume"}

// billingEnv lists variables that could make the CLI bill an API account, or route
// around the subscription. They are unset in every invocation, whatever the image
// or an operator's Dockerfile sets.
var billingEnv = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_API_KEY",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_API_KEY_HELPER_TTL_MS",
}

// Options configures the adapter.
type Options struct {
	Image            string
	Executable       string
	ConfigVolume     string
	OAuthToken       core.Secret // optional, from `claude setup-token`
	ResourceDefaults core.Resources
}

// Agent implements core.AgentProvider.
type Agent struct {
	name string
	opts Options

	mu       sync.Mutex
	verified map[string]bool
}

// New returns an agent.
func New(name string, o Options) *Agent {
	if o.Executable == "" {
		o.Executable = "claude"
	}
	if o.ConfigVolume == "" {
		o.ConfigVolume = "goblins-claude-config"
	}
	return &Agent{name: name, opts: o, verified: map[string]bool{}}
}

// Register adds the adapter to a registry.
func Register(r *providers.Registry) {
	r.Register(providers.KindAgent, typeName, func(name string, deps providers.Deps, opts providers.Options) (any, error) {
		if opts.String("api_key") != "" || opts.String("api_key_env") != "" || opts.String("api_key_file") != "" {
			return nil, core.Errorf(core.KindInvalid, "claude.config", "model API keys are not supported; use a subscription login")
		}
		tok, err := opts.Secret(deps, "oauth_token")
		if err != nil {
			return nil, err
		}
		if opts.String("image") == "" {
			return nil, core.Errorf(core.KindInvalid, "claude.config", "image is required")
		}
		return New(name, Options{
			Image: opts.String("image"), Executable: opts.String("executable"), ConfigVolume: opts.String("config_volume"), OAuthToken: tok,
			ResourceDefaults: core.Resources{MemoryBytes: int64(opts.Int("memory_mb", 4096)) << 20, CPUs: float64(opts.Int("cpus", 2)), PIDs: int64(opts.Int("pids", 512))},
		}), nil
	})
}

// Info implements core.Provider.
func (a *Agent) Info() core.ProviderInfo {
	return core.ProviderInfo{Kind: core.ProviderAgent, Type: typeName, Name: a.name, ContractVersion: core.ContractVersion,
		Capabilities: []core.Capability{core.CapAgentResume, core.CapAgentStreaming, core.CapAgentEffort}}
}

// Models implements core.AgentProvider. The CLI has no way to list models, and
// aliases change over time, so the configured value is passed through unverified.
func (a *Agent) Models(context.Context) ([]core.Model, error) { return []core.Model{}, nil }

// shellScript wraps the CLI: it clears billing variables, then runs the command
// with the prompt on stdin, which keeps the prompt out of the process listing.
func (a *Agent) shellScript(args []string) string {
	var b strings.Builder
	b.WriteString("unset " + strings.Join(billingEnv, " ") + "; ")
	b.WriteString(`if [ -n "$GOBLINS_PROMPT" ]; then printf '%s' "$GOBLINS_PROMPT" | `)
	cmd := shellQuote(append([]string{a.opts.Executable}, args...))
	b.WriteString(cmd + "; else " + cmd + " </dev/null; fi")
	return b.String()
}

func shellQuote(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(out, " ")
}

func (a *Agent) baseSpec(name string, cmd []string) core.WorkloadSpec {
	spec := core.WorkloadSpec{
		Name: name, Image: a.opts.Image, Command: cmd, Network: core.NetworkEgress,
		Mounts:    []core.Mount{{Volume: a.opts.ConfigVolume, Target: configMount}},
		Resources: a.opts.ResourceDefaults,
		Security:  core.SecurityProfile{ReadOnlyRoot: true},
		Env: map[string]string{
			"CLAUDE_CONFIG_DIR": configMount, "HOME": "/tmp", "DISABLE_AUTOUPDATER": "1",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		},
	}
	if !a.opts.OAuthToken.Empty() {
		spec.Secrets = map[string]core.Secret{"CLAUDE_CODE_OAUTH_TOKEN": a.opts.OAuthToken}
	}
	return spec
}

// args builds the CLI invocation. Tool names come from the abstract policy alone,
// so neither configuration nor a repository can add a command-running tool.
func args(req core.AgentRequest, hasEffort bool) []string {
	tools := []string{"Read", "Glob", "Grep"}
	if req.Policy.WriteFiles {
		tools = append(tools, "Edit", "Write")
	}
	if req.Policy.Web {
		tools = append(tools, "WebFetch", "WebSearch")
	}
	list := strings.Join(tools, ",")
	out := []string{
		"-p", "--output-format", "stream-json", "--verbose",
		"--restricted", "--strict-mcp-config", "--disable-slash-commands",
		"--tools", list, "--allowedTools", list, "--permission-mode", "dontAsk",
	}
	if req.Model != "" {
		out = append(out, "--model", req.Model)
	}
	if req.Effort != "" && hasEffort {
		out = append(out, "--effort", req.Effort)
	}
	if req.SystemPrompt != "" {
		out = append(out, "--append-system-prompt", req.SystemPrompt)
	}
	if req.ResumeSessionID != "" {
		out = append(out, "--resume", req.ResumeSessionID)
	}
	return out
}

// Run implements core.AgentProvider.
func (a *Agent) Run(ctx context.Context, req core.AgentRequest, sink func(core.AgentEvent)) (core.AgentResult, error) {
	if req.Runtime == nil {
		return core.AgentResult{}, core.Errorf(core.KindInvalid, "claude.run", "no runtime")
	}
	if err := a.preflight(ctx, req); err != nil {
		return core.AgentResult{}, err
	}
	name := req.WorkloadName
	if name == "" {
		name = "goblins-agent-" + core.Digest(req.IdempotencyKey)[:16]
	}
	spec := a.baseSpec(name, []string{"sh", "-c", a.shellScript(args(req, true))})
	spec.Env["GOBLINS_PROMPT"] = req.Prompt
	spec.Labels = req.Labels
	spec.Timeout = req.Timeout
	spec.WorkDir = req.WorkDir
	if req.Workspace.Volume != "" {
		w := req.Workspace
		w.ReadOnly = w.ReadOnly || !req.Policy.WriteFiles
		spec.Mounts = append(spec.Mounts, w)
	}

	if _, err := req.Runtime.Create(ctx, spec); err != nil {
		return core.AgentResult{}, err
	}
	defer cleanup(ctx, req.Runtime, name)

	parser := newStreamParser(sink)
	stderr := &tailBuffer{max: maxStderr}
	lines := &lineWriter{fn: parser.line}
	logErr := req.Runtime.Logs(ctx, name, true, lines, stderr)
	lines.flush()
	if err := ctx.Err(); err != nil {
		return core.AgentResult{SessionID: parser.session}, core.Wrap(core.KindCancelled, "claude.run", err)
	}
	if logErr != nil {
		return core.AgentResult{SessionID: parser.session}, core.Wrap(core.KindTransient, "claude.logs", logErr)
	}
	st, err := waitDone(ctx, req.Runtime, name)
	if err != nil {
		return core.AgentResult{SessionID: parser.session}, err
	}
	return parser.finish(st, stderr.String())
}

func cleanup(ctx context.Context, rt core.ExecutionRuntime, name string) {
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_ = rt.Cancel(bg, name)
	_ = rt.Cleanup(bg, name)
}

func waitDone(ctx context.Context, rt core.ExecutionRuntime, name string) (core.WorkloadStatus, error) {
	for {
		st, err := rt.Status(ctx, name)
		if err != nil {
			return st, err
		}
		if st.Phase.Terminal() {
			return st, nil
		}
		select {
		case <-ctx.Done():
			return st, core.Wrap(core.KindCancelled, "claude.wait", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// preflight verifies once per image that the installed CLI supports every flag the
// security model depends on. This fails closed, and uses no model tokens.
func (a *Agent) preflight(ctx context.Context, req core.AgentRequest) error {
	a.mu.Lock()
	ok := a.verified[a.opts.Image]
	a.mu.Unlock()
	if ok {
		return nil
	}
	out, _, err := a.runShort(ctx, req.Runtime, "goblins-preflight-"+core.Digest(req.WorkloadName, req.IdempotencyKey)[:16], []string{"--help"})
	if err != nil {
		return err
	}
	var missing []string
	for _, f := range requiredFlags {
		if !strings.Contains(out, f) {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return core.Errorf(core.KindUnsupported, "claude.preflight", "the installed Claude CLI lacks required options (%s). Goblins will not run it, because these options enforce its security boundary. Upgrade the agent image", strings.Join(missing, ", "))
	}
	a.mu.Lock()
	a.verified[a.opts.Image] = true
	a.mu.Unlock()
	return nil
}

func (a *Agent) runShort(ctx context.Context, rt core.ExecutionRuntime, name string, cliArgs []string) (string, core.WorkloadStatus, error) {
	spec := a.baseSpec(name, []string{"sh", "-c", a.shellScript(cliArgs)})
	spec.Timeout = preflightWindow
	if _, err := rt.Create(ctx, spec); err != nil {
		return "", core.WorkloadStatus{}, err
	}
	defer cleanup(ctx, rt, name)
	var out, errb tailBuffer
	out.max, errb.max = maxStderr, maxStderr
	if err := rt.Logs(ctx, name, true, &out, &errb); err != nil {
		return "", core.WorkloadStatus{}, core.Wrap(core.KindTransient, "claude.short", err)
	}
	st, err := waitDone(ctx, rt, name)
	if err != nil {
		return "", st, err
	}
	return out.String() + errb.String(), st, nil
}

// AuthStatus implements core.AgentProvider. It runs no model call, so it can back
// health checks without spending anything. A login that is not a subscription is
// reported as unusable, because using it would bill an API account.
func (a *Agent) AuthStatus(ctx context.Context, rt core.ExecutionRuntime) (core.AuthStatus, error) {
	name := fmt.Sprintf("goblins-auth-%s-%d", a.name, time.Now().UnixNano())
	out, st, err := a.runShort(ctx, rt, name, []string{"auth", "status", "--json"})
	if err != nil {
		return core.AuthStatus{State: core.AuthUnknown, Detail: core.UserMessage(err)}, err
	}
	var res struct {
		LoggedIn    bool   `json:"loggedIn"`
		AuthMethod  string `json:"authMethod"`
		APIProvider string `json:"apiProvider"`
	}
	if jerr := json.Unmarshal(firstJSONObject(out), &res); jerr != nil {
		if st.ExitCode != 0 {
			return core.AuthStatus{State: core.AuthLoginRequired, Detail: loginHint}, nil
		}
		return core.AuthStatus{State: core.AuthUnknown, Detail: "unreadable auth status"}, nil
	}
	switch {
	case !res.LoggedIn:
		return core.AuthStatus{State: core.AuthLoginRequired, Detail: loginHint}, nil
	case !subscriptionMethod(res.AuthMethod) || (res.APIProvider != "" && res.APIProvider != "firstParty"):
		return core.AuthStatus{State: core.AuthLoginRequired, Detail: fmt.Sprintf("the CLI is logged in with %q, which is not a subscription. Goblins never uses API billing. %s", res.AuthMethod, loginHint)}, nil
	}
	return core.AuthStatus{State: core.AuthOK, Detail: res.AuthMethod}, nil
}

const loginHint = "Log in on this installation: docker compose run --rm -it agent-login"

func subscriptionMethod(m string) bool {
	return m == "claude.ai" || m == "oauth_token"
}

func firstJSONObject(s string) []byte {
	i := strings.IndexByte(s, '{')
	j := strings.LastIndexByte(s, '}')
	if i < 0 || j < i {
		return nil
	}
	return []byte(s[i : j+1])
}

// streamParser turns stream-json lines into events and a final result.
type streamParser struct {
	sink       func(core.AgentEvent)
	session    string
	announced  bool
	result     *resultLine
	apiBilling string
	assistErr  string
}

func newStreamParser(sink func(core.AgentEvent)) *streamParser { return &streamParser{sink: sink} }

type resultLine struct {
	Subtype        string `json:"subtype"`
	IsError        bool   `json:"is_error"`
	Result         string `json:"result"`
	SessionID      string `json:"session_id"`
	APIErrorStatus int    `json:"api_error_status"`
}

type streamLine struct {
	Type         string `json:"type"`
	Subtype      string `json:"subtype"`
	SessionID    string `json:"session_id"`
	APIKeySource string `json:"apiKeySource"`
	Error        string `json:"error"`
	Message      struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
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
	if l.SessionID != "" {
		p.session = l.SessionID
		if !p.announced {
			p.announced = true
			p.sink(core.AgentEvent{Kind: core.AgentEventSession, SessionID: l.SessionID})
		}
	}
	switch l.Type {
	case "system":
		if l.Subtype == "init" && apiBilled(l.APIKeySource) {
			p.apiBilling = l.APIKeySource
		}
	case "assistant":
		if l.Error != "" {
			p.assistErr = l.Error
		}
		for _, c := range l.Message.Content {
			switch c.Type {
			case "text":
				p.sink(core.AgentEvent{Kind: core.AgentEventText, SessionID: p.session, Text: c.Text})
			case "tool_use":
				p.sink(core.AgentEvent{Kind: core.AgentEventTool, SessionID: p.session, Text: c.Name})
			}
		}
	case "result":
		var r resultLine
		if json.Unmarshal(raw, &r) == nil {
			p.result = &r
			if r.SessionID != "" {
				p.session = r.SessionID
			}
		}
	}
}

// apiBilled recognizes key sources that are not a subscription. The value "none"
// is what a subscription login reports.
func apiBilled(src string) bool {
	return src != "" && src != "none" && !strings.EqualFold(src, "oauth")
}

func (p *streamParser) finish(st core.WorkloadStatus, stderr string) (core.AgentResult, error) {
	res := core.AgentResult{SessionID: p.session}
	if p.apiBilling != "" {
		return res, core.Errorf(core.KindAuthRequired, "claude.run", "the CLI authenticated with %q instead of a subscription login, so the result was discarded. Remove that credential from the installation", p.apiBilling)
	}
	if p.result != nil && !p.result.IsError && st.ExitCode == 0 {
		res.Output = p.result.Result
		p.sink(core.AgentEvent{Kind: core.AgentEventResult, SessionID: p.session, Text: res.Output})
		return res, nil
	}
	text := p.assistErr + " " + stderr
	status := 0
	if p.result != nil {
		text = p.result.Result + " " + p.result.Subtype + " " + text
		status = p.result.APIErrorStatus
	}
	return res, classify(status, text, st)
}

var (
	quotaPattern  = regexp.MustCompile(`(?i)usage limit|rate[ _-]?limit|limit reached|hit your .*limit|quota|out of extra usage|billing_error`)
	authPattern   = regexp.MustCompile(`(?i)/login|not logged in|authentication_failed|authentication failed|invalid api key|oauth token|token (has )?expired|credentials? (is|are|has|have)|unauthorized`)
	resumePattern = regexp.MustCompile(`(?i)no conversation found|session .*not found`)
)

// classify maps a failure onto workflow-relevant kinds. Messages are fixed strings
// rather than CLI output, which could contain anything.
func classify(status int, text string, st core.WorkloadStatus) error {
	switch {
	case status == 401 || status == 403 || authPattern.MatchString(text):
		return core.Errorf(core.KindAuthRequired, "claude.run", "the subscription login is missing or expired. %s", loginHint)
	case status == 429 || quotaPattern.MatchString(text):
		return core.Errorf(core.KindQuotaExhausted, "claude.run", "the subscription usage limit was reached")
	case resumePattern.MatchString(text):
		return core.Errorf(core.KindNotFound, "claude.run", "the agent session can no longer be resumed")
	case st.Phase == core.WorkloadTimedOut:
		return core.Errorf(core.KindTimeout, "claude.run", "the agent run exceeded its time limit")
	case st.Phase == core.WorkloadCancelled:
		return core.Errorf(core.KindCancelled, "claude.run", "the agent run was cancelled")
	}
	return core.Errorf(core.KindTransient, "claude.run", "the agent exited unexpectedly (exit %d)", st.ExitCode)
}

// lineWriter splits a byte stream into lines, dropping absurdly long ones.
type lineWriter struct {
	fn   func([]byte)
	buf  bytes.Buffer
	skip bool
}

func (l *lineWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if !l.skip {
				l.buf.Write(p)
				if l.buf.Len() > maxLine {
					l.buf.Reset()
					l.skip = true
				}
			}
			return n, nil
		}
		if !l.skip {
			l.buf.Write(p[:i])
			l.fn(l.buf.Bytes())
		}
		l.buf.Reset()
		l.skip = false
		p = p[i+1:]
	}
	return n, nil
}

func (l *lineWriter) flush() {
	if l.buf.Len() > 0 && !l.skip {
		l.fn(l.buf.Bytes())
	}
	l.buf.Reset()
}

// tailBuffer keeps the last max bytes written.
type tailBuffer struct {
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }

var (
	_ io.Writer = (*lineWriter)(nil)
)
