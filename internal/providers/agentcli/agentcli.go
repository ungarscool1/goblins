// Package agentcli holds what adapters running a coding agent CLI in a workload have in
// common. It knows workloads and byte streams, and nothing about any particular CLI.
package agentcli

import (
	"bytes"
	"context"
	"strings"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// MaxLine bounds one line of a CLI's event stream.
const MaxLine = 32 << 20

// ShellQuote renders arguments for sh.
func ShellQuote(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(out, " ")
}

// Script wraps a CLI invocation: it clears billing variables, then runs the command with
// the prompt on stdin, which keeps the prompt out of the process listing.
func Script(billingEnv []string, cmd []string) string {
	var b strings.Builder
	b.WriteString("unset " + strings.Join(billingEnv, " ") + "; ")
	b.WriteString(`if [ -n "$GOBLINS_PROMPT" ]; then printf '%s' "$GOBLINS_PROMPT" | `)
	quoted := ShellQuote(cmd)
	b.WriteString(quoted + "; else " + quoted + " </dev/null; fi")
	return b.String()
}

// Cleanup stops and removes a workload, even when ctx is already cancelled.
func Cleanup(ctx context.Context, rt core.ExecutionRuntime, name string) {
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_ = rt.Cancel(bg, name)
	_ = rt.Cleanup(bg, name)
}

// WaitDone blocks until a workload reaches a terminal phase.
func WaitDone(ctx context.Context, rt core.ExecutionRuntime, name string) (core.WorkloadStatus,
	error) {
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
			return st, core.Wrap(core.KindCancelled, "agentcli.wait", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// LineWriter splits a byte stream into lines, dropping absurdly long ones.
type LineWriter struct {
	Fn   func([]byte)
	buf  bytes.Buffer
	skip bool
}

func (l *LineWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if !l.skip {
				l.buf.Write(p)
				if l.buf.Len() > MaxLine {
					l.buf.Reset()
					l.skip = true
				}
			}
			return n, nil
		}
		if !l.skip {
			l.buf.Write(p[:i])
			l.Fn(l.buf.Bytes())
		}
		l.buf.Reset()
		l.skip = false
		p = p[i+1:]
	}
	return n, nil
}

// Flush delivers a last line that had no newline.
func (l *LineWriter) Flush() {
	if l.buf.Len() > 0 && !l.skip {
		l.Fn(l.buf.Bytes())
	}
	l.buf.Reset()
}
