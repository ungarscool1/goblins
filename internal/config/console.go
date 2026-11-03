package config

import (
	"net"
	"time"
)

// Console configures the operator console: a web page and a JSON API to watch tasks, read
// their conversations and act on them. It has a port of its own and is off unless Listen
// is set. See docs/adr/0006-operator-console.md.
type Console struct {
	Listen string `yaml:"listen,omitempty"`
	// ReadOnly refuses every action, whatever an operator's role.
	ReadOnly bool `yaml:"read_only,omitempty"`
	// SessionTTL is how long a browser stays logged in.
	SessionTTL time.Duration `yaml:"session_ttl,omitempty"`
	// InsecureCookie drops the Secure flag of the session cookie, for a console reached over
	// plain HTTP on another host than localhost. Use TLS instead wherever possible.
	InsecureCookie bool       `yaml:"insecure_cookie,omitempty"`
	Operators      []Operator `yaml:"operators,omitempty"`
}

// Operator is one person, or one automation, allowed into the console. The token is given
// by reference only, like every other secret.
type Operator struct {
	Name      string `yaml:"name"`
	Role      string `yaml:"role"`
	TokenEnv  string `yaml:"token_env,omitempty"`
	TokenFile string `yaml:"token_file,omitempty"`
}

// Console roles. A viewer reads everything an operator reads and changes nothing.
const (
	RoleViewer   = "viewer"
	RoleOperator = "operator"
)

func (c *Config) validateConsole(add func(string, ...any)) {
	con := c.Console
	if con.Listen == "" {
		return
	}
	if _, _, err := net.SplitHostPort(con.Listen); err != nil {
		add("console.listen: %v", err)
	} else if con.Listen == c.Server.Listen || con.Listen == c.Server.MetricsListen {
		add("console.listen: must differ from server.listen and server.metrics_listen")
	}
	if con.SessionTTL < 0 {
		add("console.session_ttl: cannot be negative")
	}
	if len(con.Operators) == 0 {
		add("console.operators: at least one is required when console.listen is set")
	}
	seen := map[string]bool{}
	for i, op := range con.Operators {
		if !nameRE.MatchString(op.Name) || seen[op.Name] {
			add("console.operators[%d].name: invalid or repeated name %q", i, op.Name)
		}
		seen[op.Name] = true
		if op.Role != RoleViewer && op.Role != RoleOperator {
			add("console.operators[%d].role: must be %s or %s", i, RoleViewer, RoleOperator)
		}
		if (op.TokenEnv == "") == (op.TokenFile == "") {
			add("console.operators[%d]: set exactly one of token_env and token_file", i)
		}
	}
}
