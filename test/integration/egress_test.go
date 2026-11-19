//go:build docker

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/runtime/docker"
)

func dockerCLI(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Behind the egress proxy, an agent's network has no way out but the proxy, and the proxy
// opens tunnels to listed hosts only.
func TestEgressGoesThroughTheProxyOnly(t *testing.T) {
	engine(t) // skips without an engine
	stamp := strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	network, proxy := "goblins-test-egress-"+stamp, "goblins-test-proxy-"+stamp
	host := os.Getenv("DOCKER_HOST")
	if home, _ := os.UserHomeDir(); host == "" && home != "" {
		if _, err := os.Stat(home + "/.docker/run/docker.sock"); err == nil {
			host = "unix://" + home + "/.docker/run/docker.sock"
		}
	}
	r, err := docker.New("docker", docker.Options{Host: host, PullMissing: true,
		EgressProxy: "http://" + proxy + ":3128", EgressNetwork: network})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", proxy).Run()
		_ = exec.Command("docker", "network", "rm", network).Run()
	})

	// The proxy is this repository's binary, on the default bridge and on the internal network.
	bin := filepath.Join(t.TempDir(), "goblins")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/goblins")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux",
		"GOARCH="+dockerCLI(t, "version", "--format", "{{.Server.Arch}}"))
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	run := func(name, script string) (core.WorkloadStatus, string) {
		t.Helper()
		st, out, errb := run(t, r, core.WorkloadSpec{Name: name, Image: testImage,
			Command: []string{"sh", "-c", script}, Network: core.NetworkEgress})
		return st, out + errb
	}
	// The first egress workload creates the internal network.
	_, out := run("egress-direct-"+stamp, `env | grep -c '^HTTPS_PROXY=http://'; `+
		`wget -q -T 3 -O /dev/null http://1.1.1.1 && echo DIRECT-OPEN; echo done`)
	if strings.Contains(out, "DIRECT-OPEN") || !strings.HasPrefix(out, "1") {
		t.Fatalf("direct egress:\n%s", out)
	}
	internal := dockerCLI(t, "network", "inspect", "-f", "{{.Internal}}", network)
	if internal != "true" {
		t.Fatalf("network internal = %s", internal)
	}
	// As in Compose: the proxy's own network is a user-defined one with a route out.
	outside := network + "-out"
	dockerCLI(t, "network", "create", outside)
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", outside).Run() })
	dockerCLI(t, "run", "-d", "--name", proxy, "--network", outside, "--read-only",
		"-v", bin+":/goblins:ro", testImage,
		"/goblins", "egress-proxy", "--allow", "example.com")
	dockerCLI(t, "network", "connect", network, proxy)
	time.Sleep(time.Second)
	tunnel := func(target string) string {
		// stdin stays open, as a real client's does: closing it would end the request.
		return `(printf 'CONNECT ` + target + ` HTTP/1.1\r\nHost: ` + target + `\r\n\r\n'; ` +
			`sleep 4) | nc -w 5 ` + proxy + ` 3128 | head -1`
	}
	_, allowed := run("egress-allowed-"+stamp, tunnel("example.com:443"))
	_, denied := run("egress-denied-"+stamp, tunnel("github.com:443"))
	_, inside := run("egress-inside-"+stamp, tunnel("169.254.169.254:80"))
	if !strings.Contains(allowed, " 200") {
		t.Errorf("listed host: %q (needs this machine to reach example.com)", allowed)
	}
	for name, got := range map[string]string{"unlisted host": denied, "metadata address": inside} {
		if !strings.Contains(got, " 403") {
			t.Errorf("%s: %q", name, got)
		}
	}
	logs := dockerCLI(t, "logs", proxy)
	if !strings.Contains(logs, "egress denied") {
		t.Errorf("proxy log:\n%s", logs)
	}
}

// A check reaches its services by name on a network only they share, still reaches nothing
// else, and its services go with it.
func TestServicesShareAPrivateNetwork(t *testing.T) {
	r := engine(t)
	st, out, errb := run(t, r, core.WorkloadSpec{Name: "with-db", Network: core.NetworkNone,
		Command: []string{"sh", "-c", `for i in 1 2 3 4 5 6 7 8; do nc -w 2 db 8080 && break; ` +
			`sleep 1; done; wget -q -T 3 -O /dev/null http://1.1.1.1 && echo NETWORK-OPEN; ` +
			`echo done`},
		Services: []core.Service{{Name: "db", Image: testImage, Command: []string{"sh", "-c",
			"while true; do echo pong | nc -l -p 8080; done"}}}})
	if st.Phase != core.WorkloadSucceeded || !strings.Contains(out, "pong") ||
		strings.Contains(out, "NETWORK-OPEN") {
		t.Fatalf("status %+v\n%s%s", st, out, errb)
	}
	name := ""
	for _, l := range strings.Split(dockerCLI(t, "ps", "-a", "--format", "{{.Names}}"), "\n") {
		if strings.Contains(l, "with-db") && strings.Contains(l, "-svc-db") {
			name = strings.TrimSuffix(l, "-svc-db")
		}
	}
	if name == "" {
		t.Fatal("no service container while the workload exists")
	}
	if err := r.Cleanup(t.Context(), name); err != nil {
		t.Fatal(err)
	}
	if left := dockerCLI(t, "ps", "-a", "-q", "--filter",
		"label=goblins.workload="+name); left != "" {
		t.Fatalf("service containers left: %s", left)
	}
	if out, err := exec.Command("docker", "network", "inspect",
		name+"-services").CombinedOutput(); err == nil {
		t.Fatalf("services network left:\n%s", out)
	}
}
