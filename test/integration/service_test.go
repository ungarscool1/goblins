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

type service struct {
	t    *testing.T
	bin  string
	addr string
	data string
	cmd  *exec.Cmd
	logs *strings.Builder
}

func (s *service) env() []string {
	return append(os.Environ(), "GOBLINS_LISTEN="+s.addr, "GOBLINS_DATA_DIR="+s.data)
}

func (s *service) start() {
	s.t.Helper()
	s.logs = &strings.Builder{}
	s.cmd = exec.Command(s.bin, "serve", "--mock")
	s.cmd.Stderr = s.logs
	s.cmd.Env = s.env()
	if err := s.cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	s.eventually(func() bool {
		resp, err := http.Get("http://" + s.addr + "/readyz")
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode == 200
	})
}

func (s *service) stop() {
	s.t.Helper()
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		s.t.Fatal(err)
	}
	if err := s.cmd.Wait(); err != nil {
		s.t.Fatalf("exit: %v\n%s", err, s.logs)
	}
}

func (s *service) eventually(cond func() bool) {
	s.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			s.t.Fatalf("condition not met; logs:\n%s", s.logs)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (s *service) post(path, body string) map[string]any {
	s.t.Helper()
	resp, err := http.Post("http://"+s.addr+path, "application/json", strings.NewReader(body))
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		s.t.Fatalf("%s: %d", path, resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func (s *service) status() string {
	s.t.Helper()
	cmd := exec.Command(s.bin, "status", "--mock")
	cmd.Env = s.env()
	out, err := cmd.Output()
	if err != nil {
		s.t.Fatal(err)
	}
	return string(out)
}

// A person can try the whole workflow by hand in mock mode, and state survives a restart.
func TestMockWorkflowAcrossRestart(t *testing.T) {
	s := &service{t: t, bin: buildBinary(t), addr: freeAddr(t), data: t.TempDir()}
	s.start()
	issue := s.post("/mock/demo/issues", `{"title":"Try it","body":"Do the thing"}`)["issue"].(string)
	s.eventually(func() bool { return strings.Contains(s.status(), "design-proposed") })

	s.stop()
	if !strings.Contains(s.status(), "design-proposed") {
		t.Fatalf("state lost while stopped: %s", s.status())
	}
	s.start()
	defer s.stop()
	if !strings.Contains(s.status(), "design-proposed") {
		t.Fatalf("state lost across restart: %s", s.status())
	}

	// The fake tracker's contents are memory only, but an operator can still act.
	fields := strings.Fields(s.status())
	cmd := exec.Command(s.bin, "comment", "--mock", "--task", fields[0], "--as", "alice", "--body", "/approve-design 99")
	cmd.Env = s.env()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("comment: %v %s", err, out)
	}
	time.Sleep(200 * time.Millisecond)
	if !strings.Contains(s.status(), "design-proposed") {
		t.Fatalf("wrong revision approved: %s", s.status())
	}
	_ = issue
}

func TestMockWorkflowToCompletion(t *testing.T) {
	s := &service{t: t, bin: buildBinary(t), addr: freeAddr(t), data: t.TempDir()}
	s.start()
	defer s.stop()
	issue := s.post("/mock/demo/issues", `{"title":"Try it","body":"Do the thing"}`)["issue"].(string)
	s.eventually(func() bool { return strings.Contains(s.status(), "design-proposed") })

	s.post("/mock/demo/issues/"+issue+"/comments", `{"body":"/approve-design 1"}`)
	s.eventually(func() bool { return strings.Contains(s.status(), "completed") })

	resp, err := http.Get("http://" + s.addr + "/mock/demo/issues/" + issue + "/comments")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var comments []struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&comments); err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, c := range comments {
		all.WriteString(c.Body + "\n")
	}
	for _, want := range []string{"Proposed design, revision 1", "approved by @operator", "Pull request:", "Status report", "nothing was tested"} {
		if !strings.Contains(all.String(), want) {
			t.Errorf("missing %q in:\n%s", want, all.String())
		}
	}
}

func TestDoctorInMockMode(t *testing.T) {
	bin := buildBinary(t)
	cmd := exec.Command(bin, "doctor", "--mock")
	cmd.Env = append(os.Environ(), "GOBLINS_DATA_DIR="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("%v\n%s", err, out)
	}
}
