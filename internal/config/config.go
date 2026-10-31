// Package config loads, validates and renders versioned Goblins configuration.
//
// Provider adapters are declared as named instances per kind. Secrets are never
// stored in the file: options whose name designates a secret must use a *_env
// or *_file indirection, so configuration can be rendered and logged safely.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/observability"
	"github.com/ungarscool1/goblins/internal/providers"
)

// CurrentVersion is the only configuration schema version this build accepts.
const CurrentVersion = 1

// Config is the root configuration of one installation. An installation owns
// its credentials and data; separate installations share neither.
type Config struct {
	Version       int          `yaml:"version"`
	Installation  Installation `yaml:"installation"`
	Server        Server       `yaml:"server"`
	Log           Log          `yaml:"log"`
	Storage       Storage      `yaml:"storage"`
	Scheduler     Scheduler    `yaml:"scheduler,omitempty"`
	Trackers      ProviderSet  `yaml:"trackers,omitempty"`
	SCMs          ProviderSet  `yaml:"scms,omitempty"`
	EventSources  ProviderSet  `yaml:"event_sources,omitempty"`
	Agents        ProviderSet  `yaml:"agents,omitempty"`
	Runtimes      ProviderSet  `yaml:"runtimes,omitempty"`
	Reviews       ProviderSet  `yaml:"reviews,omitempty"`
	Validations   ProviderSet  `yaml:"validations,omitempty"`
	Notifications ProviderSet  `yaml:"notifications,omitempty"`
	Projects      []Project    `yaml:"projects,omitempty"`
}

// Installation identifies this deployment.
type Installation struct {
	Name    string `yaml:"name"`
	DataDir string `yaml:"data_dir"`
}

// Server configures the HTTP listener.
type Server struct {
	Listen          string        `yaml:"listen"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

// Log configures structured logging.
type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Storage selects the persistence backend. The DSN is a secret and must be
// passed by reference through DSNEnv; the sqlite driver defaults to a file in
// the data directory.
type Storage struct {
	Driver string `yaml:"driver"`
	Path   string `yaml:"path,omitempty"`
	DSNEnv string `yaml:"dsn_env,omitempty"`
}

// Scheduler limits how much runs at once. Zero means unlimited, except Workers.
type Scheduler struct {
	Workers       int `yaml:"workers,omitempty"`
	MaxGlobal     int `yaml:"max_concurrent_agents,omitempty"`
	MaxPerProject int `yaml:"max_agents_per_project,omitempty"`
}

// Provider is one named adapter instance.
type Provider struct {
	Type    string         `yaml:"type"`
	Options map[string]any `yaml:"options,omitempty"`
	// MaxConcurrent bounds simultaneous runs on one agent account. Other kinds ignore it.
	MaxConcurrent int `yaml:"max_concurrent,omitempty"`
}

// ProviderSet maps instance names to provider definitions.
type ProviderSet map[string]Provider

// Project binds one unit of work to its provider instances.
type Project struct {
	ID           string          `yaml:"id"`
	Tracker      string          `yaml:"tracker"`
	SCM          string          `yaml:"scm"`
	EventSource  []string        `yaml:"event_sources,omitempty"`
	Repository   string          `yaml:"repository"`
	BaseBranch   string          `yaml:"base_branch,omitempty"`
	Validation   string          `yaml:"validation,omitempty"`
	Review       string          `yaml:"review,omitempty"`
	Notification string          `yaml:"notification,omitempty"`
	Stages       Stages          `yaml:"stages"`
	Policy       Policy          `yaml:"policy,omitempty"`
	Workspace    WorkspaceConfig `yaml:"workspace,omitempty"`
	// PollInterval enables reconciliation polling on the project's event sources.
	PollInterval time.Duration `yaml:"poll_interval,omitempty"`
}

// Policy overrides workflow rules. Unset fields keep safe defaults.
type Policy struct {
	ApproveCommand       string        `yaml:"approve_command,omitempty"`
	MinApprovePermission string        `yaml:"min_approve_permission,omitempty"`
	MinInputPermission   string        `yaml:"min_input_permission,omitempty"`
	IntakeLabel          string        `yaml:"intake_label,omitempty"`
	ReviewPullRequests   bool          `yaml:"review_pull_requests,omitempty"`
	BotLogins            []string      `yaml:"bot_logins,omitempty"`
	MaxPlanLoops         int           `yaml:"max_plan_loops,omitempty"`
	MaxImplementLoops    int           `yaml:"max_implement_loops,omitempty"`
	QuotaPause           time.Duration `yaml:"quota_pause,omitempty"`
	AuthPause            time.Duration `yaml:"auth_pause,omitempty"`
	WebResearch          bool          `yaml:"web_research,omitempty"`
	// AllowUnvalidatedDraftPR lets a project without validation open draft pull requests
	// from commits nothing has tested. Without it, such a project is refused.
	AllowUnvalidatedDraftPR bool `yaml:"allow_unvalidated_draft_pr,omitempty"`
	// AllowSkippedReview lets a project without a reviewer go on without independent review.
	AllowSkippedReview bool `yaml:"allow_skipped_review,omitempty"`
}

// WorkspaceConfig configures Git workspaces.
type WorkspaceConfig struct {
	GitImage    string `yaml:"git_image,omitempty"`
	CommitName  string `yaml:"commit_name,omitempty"`
	CommitEmail string `yaml:"commit_email,omitempty"`
	Signoff     bool   `yaml:"signoff,omitempty"`
}

// Stages chooses the agent and runtime per workflow stage.
type Stages struct {
	Planner     Stage `yaml:"planner"`
	Implementer Stage `yaml:"implementer"`
}

// Stage configures one agent role.
type Stage struct {
	Agent   string        `yaml:"agent"`
	Runtime string        `yaml:"runtime"`
	Model   string        `yaml:"model,omitempty"`
	Effort  string        `yaml:"effort,omitempty"`
	Timeout time.Duration `yaml:"timeout,omitempty"`
}

// Default returns a configuration with defaults filled in.
func Default() Config {
	return Config{
		Version:      CurrentVersion,
		Installation: Installation{Name: "default", DataDir: "./data"},
		Server:       Server{Listen: "127.0.0.1:8080", ShutdownTimeout: 20 * time.Second},
		Log:          Log{Level: "info", Format: "json"},
		Storage:      Storage{Driver: "sqlite"},
	}
}

// Parse decodes YAML strictly on top of the defaults: unknown fields, multiple
// documents and missing versions are errors.
func Parse(data []byte) (Config, error) {
	cfg := Default()
	cfg.Version = 0
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, core.Wrap(core.KindInvalid, "config.parse", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Config{}, core.Errorf(core.KindInvalid, "config.parse", "configuration must be a single YAML document")
	}
	return cfg, nil
}

// Load reads path, applies environment overrides from env and validates the
// result against known provider types.
func Load(path string, env func(string) (string, bool), known TypeChecker) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, core.Wrap(core.KindInvalid, "config.load", err)
	}
	return LoadBytes(data, env, known)
}

// LoadBytes is Load for in-memory configuration.
func LoadBytes(data []byte, env func(string) (string, bool), known TypeChecker) (Config, error) {
	cfg, err := Parse(data)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.applyEnv(env); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(known); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// applyEnv applies the documented GOBLINS_* overrides.
func (c *Config) applyEnv(env func(string) (string, bool)) error {
	if env == nil {
		return nil
	}
	set := func(name string, dst *string) {
		if v, ok := env(name); ok {
			*dst = v
		}
	}
	set("GOBLINS_INSTALLATION_NAME", &c.Installation.Name)
	set("GOBLINS_DATA_DIR", &c.Installation.DataDir)
	set("GOBLINS_LISTEN", &c.Server.Listen)
	set("GOBLINS_LOG_LEVEL", &c.Log.Level)
	set("GOBLINS_LOG_FORMAT", &c.Log.Format)
	set("GOBLINS_STORAGE_DRIVER", &c.Storage.Driver)
	set("GOBLINS_STORAGE_PATH", &c.Storage.Path)
	if v, ok := env("GOBLINS_SHUTDOWN_TIMEOUT"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return core.Errorf(core.KindInvalid, "config.env", "GOBLINS_SHUTDOWN_TIMEOUT: invalid duration %s", strconv.Quote(v))
		}
		c.Server.ShutdownTimeout = d
	}
	return nil
}

// TypeChecker reports whether an adapter type is available for a kind.
type TypeChecker func(kind providers.Kind, typ string) bool

var (
	nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9_-]{0,62}[a-z0-9])?$`)
	// Option names that may hold a secret reference rather than a secret.
	secretRefRE = regexp.MustCompile(`_(env|file)$`)
)

// Validate returns all configuration problems at once.
func (c *Config) Validate(known TypeChecker) error {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	if c.Version != CurrentVersion {
		add("version: unsupported value %d (this build supports %d)", c.Version, CurrentVersion)
	}
	if !nameRE.MatchString(c.Installation.Name) {
		add("installation.name: invalid name %q", c.Installation.Name)
	}
	if c.Installation.DataDir == "" {
		add("installation.data_dir: required")
	}
	if c.Server.Listen == "" {
		add("server.listen: required")
	}
	if c.Server.ShutdownTimeout <= 0 {
		add("server.shutdown_timeout: must be positive")
	}
	if _, err := observability.NewLogger(io.Discard, observability.LogOptions{Level: c.Log.Level, Format: c.Log.Format}); err != nil {
		add("log: %v", err)
	}
	switch c.Storage.Driver {
	case "sqlite":
	case "postgres":
		if c.Storage.DSNEnv == "" {
			add("storage.dsn_env: required for the postgres driver")
		}
	default:
		add("storage.driver: unknown driver %q (want sqlite or postgres)", c.Storage.Driver)
	}

	for _, s := range c.sets() {
		for _, name := range slices.Sorted(maps.Keys(s.set)) {
			p := s.set[name]
			path := fmt.Sprintf("%s.%s", s.field, name)
			if !nameRE.MatchString(name) {
				add("%s: invalid instance name", path)
			}
			if name == "operator" || name == "engine" {
				add("%s: this instance name is reserved", path)
			}
			if p.Type == "" {
				add("%s.type: required", path)
			} else if known != nil && !known(s.kind, p.Type) {
				add("%s.type: unknown %s provider %q", path, s.kind, p.Type)
			}
			validateOptions(path+".options", p.Options, add)
		}
	}

	seen := map[string]bool{}
	for i, p := range c.Projects {
		path := fmt.Sprintf("projects[%d]", i)
		if !nameRE.MatchString(p.ID) {
			add("%s.id: invalid id %q", path, p.ID)
		}
		if seen[p.ID] {
			add("%s.id: duplicate id %q", path, p.ID)
		}
		seen[p.ID] = true
		if p.Repository == "" {
			add("%s.repository: required", path)
		}
		ref := func(field, name string, set ProviderSet, required bool) {
			if name == "" {
				if required {
					add("%s.%s: required", path, field)
				}
				return
			}
			if _, ok := set[name]; !ok {
				add("%s.%s: references undefined instance %q", path, field, name)
			}
		}
		ref("tracker", p.Tracker, c.Trackers, true)
		ref("scm", p.SCM, c.SCMs, true)
		ref("validation", p.Validation, c.Validations, false)
		ref("review", p.Review, c.Reviews, false)
		ref("notification", p.Notification, c.Notifications, false)
		if p.Validation == "" && !p.Policy.AllowUnvalidatedDraftPR {
			add("projects[%d].validation: required, unless policy.allow_unvalidated_draft_pr accepts pull requests from untested commits", i)
		}
		if p.Review == "" && !p.Policy.AllowSkippedReview {
			add("projects[%d].review: required, unless policy.allow_skipped_review accepts going without independent review", i)
		}
		for j, es := range p.EventSource {
			ref(fmt.Sprintf("event_sources[%d]", j), es, c.EventSources, true)
		}
		ref("stages.planner.agent", p.Stages.Planner.Agent, c.Agents, true)
		ref("stages.planner.runtime", p.Stages.Planner.Runtime, c.Runtimes, true)
		ref("stages.implementer.agent", p.Stages.Implementer.Agent, c.Agents, true)
		ref("stages.implementer.runtime", p.Stages.Implementer.Runtime, c.Runtimes, true)
		for _, perm := range []struct{ field, value string }{
			{"policy.min_approve_permission", p.Policy.MinApprovePermission},
			{"policy.min_input_permission", p.Policy.MinInputPermission},
		} {
			if perm.value != "" && core.ParsePermission(perm.value) < core.PermissionTriage {
				add("%s.%s: must be triage, write or admin", path, perm.field)
			}
		}
		if p.Policy.MinApprovePermission != "" && core.ParsePermission(p.Policy.MinApprovePermission) < core.PermissionWrite {
			add("%s.policy.min_approve_permission: approval needs at least write access", path)
		}
		if c := p.Policy.ApproveCommand; c != "" && !strings.HasPrefix(c, "/") {
			add("%s.policy.approve_command: must start with a slash", path)
		}
	}
	if len(errs) > 0 {
		return core.Errorf(core.KindInvalid, "config.validate", "%s", strings.Join(errs, "; "))
	}
	return nil
}

type namedSet struct {
	field string
	kind  providers.Kind
	set   ProviderSet
}

func (c *Config) sets() []namedSet {
	return []namedSet{
		{"trackers", providers.KindTracker, c.Trackers},
		{"scms", providers.KindSCM, c.SCMs},
		{"event_sources", providers.KindEventSource, c.EventSources},
		{"agents", providers.KindAgent, c.Agents},
		{"runtimes", providers.KindRuntime, c.Runtimes},
		{"reviews", providers.KindReview, c.Reviews},
		{"validations", providers.KindValidation, c.Validations},
		{"notifications", providers.KindNotification, c.Notifications},
	}
}

// validateOptions rejects inline secrets so a file is always safe to render.
func validateOptions(path string, opts map[string]any, add func(string, ...any)) {
	for _, k := range slices.Sorted(maps.Keys(opts)) {
		validateOption(path+"."+k, k, opts[k], add)
	}
}

// validateOption checks one value. Items of a sequence inherit the key that holds them.
func validateOption(path, key string, value any, add func(string, ...any)) {
	switch v := value.(type) {
	case map[string]any:
		validateOptions(path, v, add)
	case []any:
		for i, item := range v {
			validateOption(fmt.Sprintf("%s[%d]", path, i), key, item, add)
		}
	case string:
		if observability.IsSensitiveKey(key) && !secretRefRE.MatchString(key) && v != "" {
			add("%s: inline secrets are not allowed, use %s_env or %s_file", path, key, key)
		}
	}
}

// Render returns the effective configuration as YAML with anything that could
// be sensitive masked, safe for logs and support bundles.
func (c *Config) Render() ([]byte, error) {
	clone := *c
	clone.Trackers = redactSet(c.Trackers)
	clone.SCMs = redactSet(c.SCMs)
	clone.EventSources = redactSet(c.EventSources)
	clone.Agents = redactSet(c.Agents)
	clone.Runtimes = redactSet(c.Runtimes)
	clone.Reviews = redactSet(c.Reviews)
	clone.Validations = redactSet(c.Validations)
	clone.Notifications = redactSet(c.Notifications)
	return yaml.Marshal(&clone)
}

func redactSet(s ProviderSet) ProviderSet {
	if s == nil {
		return nil
	}
	out := make(ProviderSet, len(s))
	for name, p := range s {
		p.Options = redactMap(p.Options)
		out[name] = p
	}
	return out
}

func redactMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch {
		case secretRefRE.MatchString(k):
			out[k] = v // a reference, not the secret itself
		case observability.IsSensitiveKey(k):
			switch v.(type) {
			case map[string]any:
				out[k] = redactValue(v)
			default:
				out[k] = "[REDACTED]"
			}
		default:
			out[k] = redactValue(v)
		}
	}
	return out
}

func redactValue(v any) any {
	switch tv := v.(type) {
	case map[string]any:
		return redactMap(tv)
	case []any:
		out := make([]any, len(tv))
		for i, item := range tv {
			out[i] = redactValue(item)
		}
		return out
	case string:
		return observability.RedactText(tv)
	}
	return v
}
