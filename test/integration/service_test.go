//go:build integration

// Package integration exercises the built binary. Run with `make integration`.
// Tests never consume LLM tokens and never contact external services.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "goblins")
	cmd := exec.Command("go", "build", "-o", bin, "../../cmd/goblins")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

func TestMockServiceLifecycle(t *testing.T) {
	bin := buildBinary(t)
	addr := freeAddr(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var logs strings.Builder
	cmd := exec.CommandContext(ctx, bin, "serve", "--mock")
	cmd.Stderr = &logs
	cmd.Env = append(os.Environ(), "GOBLINS_LISTEN="+addr, "GOBLINS_DATA_DIR="+t.TempDir())
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	url := fmt.Sprintf("http://%s/readyz", addr)
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get(url)
		if err == nil {
			var body map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&body)
			_ = resp.Body.Close()
			if resp.StatusCode == 200 && body["status"] == "ready" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("service never became ready; logs:\n%s", logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	if out, err := exec.Command(bin, "healthcheck", "--url", url).CombinedOutput(); err != nil {
		t.Fatalf("healthcheck: %v %s", err, out)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("graceful exit failed: %v\n%s", err, logs.String())
	}
	if !strings.Contains(logs.String(), "goblins stopped") {
		t.Fatalf("missing shutdown log:\n%s", logs.String())
	}
}
