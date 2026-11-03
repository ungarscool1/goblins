package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/ungarscool1/goblins/internal/api"
	"github.com/ungarscool1/goblins/internal/config"
	"github.com/ungarscool1/goblins/internal/console"
	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/workflow"
)

// serveConsole starts the operator console when one is configured, on a port of its own: the
// port that takes webhooks serves no console, and the console serves nothing else.
func serveConsole(ctx context.Context, cfg config.Config, store core.StateStore,
	log *slog.Logger, wg *sync.WaitGroup) error {
	con := cfg.Console
	if con.Listen == "" {
		return nil
	}
	ops, err := consoleOperators(con.Operators, os.LookupEnv)
	if err != nil {
		return err
	}
	h := console.Handler(console.Options{Store: store, Operators: ops, ReadOnly: con.ReadOnly,
		SessionTTL: con.SessionTTL, Log: log,
		Policies: consolePolicies(cfg), Agents: consoleAgents(cfg)})
	ln, err := net.Listen("tcp", con.Listen)
	if err != nil {
		return core.Wrap(core.KindInvalid, "app.console_listen",
			fmt.Errorf("console.listen %s: %w", con.Listen, err))
	}
	log.Info("console listening", "listen", ln.Addr().String(), "operators", len(ops),
		"read_only", con.ReadOnly)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := api.ServeHandler(ctx, ln, h, cfg.Server.ShutdownTimeout); err != nil {
			log.Error("console listener stopped", "err", err)
		}
	}()
	return nil
}

// consoleOperators reads each operator's token. A short token is refused rather than
// accepted, since the console takes actions a tracker would ask a maintainer for.
func consoleOperators(list []config.Operator,
	env func(string) (string, bool)) ([]console.Operator, error) {
	out := make([]console.Operator, 0, len(list))
	for _, op := range list {
		token, err := operatorToken(op, env)
		if err != nil {
			return nil, err
		}
		if len(token) < console.MinTokenLength {
			return nil, core.Errorf(core.KindInvalid, "app.console",
				"console operator %s: the token must be at least %d characters",
				op.Name, console.MinTokenLength)
		}
		out = append(out, console.Operator{Name: op.Name, CanAct: op.Role == config.RoleOperator,
			Token: core.NewSecret(token)})
	}
	return out, nil
}

func operatorToken(op config.Operator, env func(string) (string, bool)) (string, error) {
	if op.TokenEnv != "" {
		v, _ := env(op.TokenEnv)
		if v == "" {
			return "", core.Errorf(core.KindInvalid, "app.console",
				"console operator %s: environment variable %s is not set", op.Name, op.TokenEnv)
		}
		return v, nil
	}
	data, err := os.ReadFile(op.TokenFile)
	if err != nil {
		return "", core.Errorf(core.KindInvalid, "app.console",
			"console operator %s: cannot read token_file", op.Name)
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

func consolePolicies(cfg config.Config) workflow.Policies {
	policies := map[string]workflow.Policy{}
	for _, pc := range cfg.Projects {
		policies[pc.ID] = policyFor(pc)
	}
	return func(p string) (workflow.Policy, bool) { v, ok := policies[p]; return v, ok }
}

// consoleAgents says who works on each project, from its configuration.
func consoleAgents(cfg config.Config) func(string) []string {
	agents := map[string][]string{}
	for _, pc := range cfg.Projects {
		s := pc.Stages
		lines := []string{stageLine("planner", s.Planner), stageLine("implementer", s.Implementer)}
		for _, r := range s.Reviewers {
			lines = append(lines, stageLine("reviewer", r))
		}
		if s.Triager != nil {
			lines = append(lines, stageLine("triager", *s.Triager))
		}
		if s.Describer != nil {
			lines = append(lines, stageLine("describer", *s.Describer))
		}
		agents[pc.ID] = lines
	}
	return func(p string) []string { return agents[p] }
}

func stageLine(role string, s config.Stage) string {
	line := role + ": " + s.Agent
	if s.Model != "" {
		line += ", model " + s.Model
	}
	if s.Effort != "" {
		line += ", effort " + s.Effort
	}
	if s.FallbackModel != "" {
		line += ", falling back to " + s.FallbackModel
	}
	return line
}
