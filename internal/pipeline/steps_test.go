package pipeline

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ungarscool1/goblins/internal/observability"
)

// A failed step's message names what failed and why, survives the redaction of messages on its
// way to the ticket, and carries no credential.
func TestAFailedStepSaysWhatAndWhy(t *testing.T) {
	stderr := "Cloning into 'repo'...\n" +
		"fatal: unable to access 'https://x-access-token:tok_abcdefghijklmnopqrstuvwxyz0123456789@" +
		"forge.example/o/r/': Failed to connect to forge.example port 443\n"
	msg := fmt.Sprintf("the %s step failed (exit 128)%s",
		stepOf("goblins-j-68396f3fcd6d13b45d1f3b3d-prepare-1"), lastLine(stderr))
	got := observability.RedactText(msg)
	if !strings.HasPrefix(got, "the prepare step failed (exit 128): fatal: unable to access") ||
		!strings.Contains(got, "Failed to connect to forge.example port 443") {
		t.Fatalf("message = %q", got)
	}
	if strings.Contains(got, "tok_") {
		t.Fatalf("a token reached the message: %q", got)
	}
	if stepOf("something-else") != "workspace" || lastLine("\n\n") != "" {
		t.Fatal("fallbacks")
	}
}
