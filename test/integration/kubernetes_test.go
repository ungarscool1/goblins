//go:build kubernetes

// Tests against a real Kubernetes API server, with the Helm chart installed. They need
// GOBLINS_K8S_URL, GOBLINS_K8S_TOKEN_FILE, GOBLINS_K8S_CA_FILE and GOBLINS_K8S_NAMESPACE,
// for the chart's control plane service account, and skip without them.
package integration

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers/contract"
	"github.com/ungarscool1/goblins/internal/runtime/kubernetes"
)

const podImage = "alpine:3.22"

func podCommand(cmd ...string) []string {
	switch cmd[0] {
	case "ok":
		return []string{"sh", "-c", "echo " + strings.Join(cmd[1:], " ")}
	case "hang":
		return []string{"sleep", "300"}
	}
	return []string{"sh", "-c", "exit 3"}
}

func cluster(t *testing.T) *kubernetes.Runtime {
	t.Helper()
	url := os.Getenv("GOBLINS_K8S_URL")
	if url == "" {
		t.Skip("GOBLINS_K8S_URL not set")
	}
	rt, err := kubernetes.New("k8s", kubernetes.Options{
		Client: kubernetes.ClientOptions{URL: url, TokenFile: os.Getenv("GOBLINS_K8S_TOKEN_FILE"),
			CAFile: os.Getenv("GOBLINS_K8S_CA_FILE")},
		Namespace: os.Getenv("GOBLINS_K8S_NAMESPACE"), ServiceAccount: "goblins-workload",
		NetworkPolicies: true, DiskLimits: true, VolumeSize: "64Mi",
		Defaults: core.Resources{MemoryBytes: 256 << 20, CPUs: 1}})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func TestKubernetesContract(t *testing.T) {
	rt := cluster(t)
	contract.ExecutionRuntime(t, func(t *testing.T) contract.RuntimeHarness {
		return contract.RuntimeHarness{Runtime: rt, Image: podImage, Translate: podCommand}
	})
}

func runPod(t *testing.T, rt *kubernetes.Runtime, spec core.WorkloadSpec) (core.WorkloadStatus,
	string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	spec.Name = "itest-" + spec.Name + "-" + strings.ReplaceAll(time.Now().Format("150405.000"),
		".", "")
	spec.Image = podImage
	t.Cleanup(func() { _ = rt.Cleanup(context.Background(), spec.Name) })
	if _, err := rt.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := rt.Logs(ctx, spec.Name, true, &out, &out); err != nil {
		t.Fatal(err)
	}
	for {
		st, err := rt.Status(ctx, spec.Name)
		if err != nil {
			t.Fatal(err)
		}
		if st.Phase.Terminal() {
			return st, out.String()
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// What a pod can reach, enforced by the cluster's network policies, and what it holds.
func TestKubernetesConfinement(t *testing.T) {
	rt := cluster(t)
	probe := `id -u; touch /x 2>/dev/null && echo ROOT-WRITABLE; touch /tmp/ok && echo TMP-OK
test -e /var/run/secrets/kubernetes.io/serviceaccount/token && echo SA-TOKEN
grep CapEff /proc/self/status
echo "secret=${STAGE_TOKEN:-none}"
wget -q -T 5 -O /dev/null http://1.1.1.1 && echo INTERNET-OPEN
wget -q -T 5 --no-check-certificate -O /dev/null https://kubernetes.default.svc 2>/dev/null; ` +
		`nc -z -w 5 10.43.0.1 443 && echo CLUSTER-OPEN
echo done`
	st, none := runPod(t, rt, core.WorkloadSpec{Name: "none", Network: core.NetworkNone,
		Command: []string{"sh", "-c", probe}, Security: core.SecurityProfile{ReadOnlyRoot: true}})
	if st.Phase != core.WorkloadSucceeded || !strings.HasPrefix(none, "10001") {
		t.Fatalf("status %+v:\n%s", st, none)
	}
	for _, bad := range []string{"ROOT-WRITABLE", "SA-TOKEN", "INTERNET-OPEN", "CLUSTER-OPEN"} {
		if strings.Contains(none, bad) {
			t.Errorf("no-network pod: %s\n%s", bad, none)
		}
	}
	for _, want := range []string{"TMP-OK", "CapEff:\t0000000000000000", "done"} {
		if !strings.Contains(none, want) {
			t.Errorf("no-network pod lacks %q:\n%s", want, none)
		}
	}
	// Egress pods have no guard (docs/kubernetes.md), so their policy may take a moment.
	st, egress := runPod(t, rt, core.WorkloadSpec{Name: "egress", Network: core.NetworkEgress,
		Command: []string{"sh", "-c", "sleep 5\n" + probe}, Trust: core.TrustCredentialed,
		Secrets: map[string]core.Secret{"STAGE_TOKEN": core.NewSecret("stg-real-123")}})
	// Behind the chart's egress proxy, egress pods reach nothing directly, the internet included.
	wantInternet := os.Getenv("GOBLINS_K8S_EGRESS_PROXY") == ""
	if st.Phase != core.WorkloadSucceeded || strings.Contains(egress,
		"INTERNET-OPEN") != wantInternet ||
		strings.Contains(egress, "CLUSTER-OPEN") || !strings.Contains(egress,
		"secret=stg-real-123") {
		t.Fatalf("egress pod %+v:\n%s", st, egress)
	}
}

// A check reaches its service by name, as a sidecar, and the pod completes.
func TestKubernetesServices(t *testing.T) {
	rt := cluster(t)
	st, out := runPod(t, rt, core.WorkloadSpec{Name: "svc", Network: core.NetworkNone,
		Command: []string{"sh", "-c",
			`for i in 1 2 3 4 5 6 7 8 9 10; do nc -w 2 db 8080 && break; ` +
				`sleep 1; done; echo done`},
		Services: []core.Service{{Name: "db", Image: podImage, Command: []string{"sh", "-c",
			"while true; do echo pong | nc -l -p 8080; done"}}}})
	if st.Phase != core.WorkloadSucceeded || !strings.Contains(out, "pong") {
		t.Fatalf("status %+v:\n%s", st, out)
	}
}

// Behind the chart's egress proxy, an egress pod reaches listed hosts through it and nothing
// directly. Needs GOBLINS_K8S_EGRESS_PROXY, with example.com allowed.
func TestKubernetesEgressProxy(t *testing.T) {
	proxy := os.Getenv("GOBLINS_K8S_EGRESS_PROXY")
	if proxy == "" {
		t.Skip("GOBLINS_K8S_EGRESS_PROXY not set")
	}
	rt := cluster(t)
	viaProxy, err := kubernetes.New("k8s-proxy", kubernetes.Options{
		Client: kubernetes.ClientOptions{URL: os.Getenv("GOBLINS_K8S_URL"),
			TokenFile: os.Getenv("GOBLINS_K8S_TOKEN_FILE"),
			CAFile:    os.Getenv("GOBLINS_K8S_CA_FILE")},
		Namespace: os.Getenv("GOBLINS_K8S_NAMESPACE"), ServiceAccount: "goblins-workload",
		NetworkPolicies: true, EgressProxy: proxy})
	if err != nil {
		t.Fatal(err)
	}
	_ = rt
	host := strings.TrimSuffix(strings.TrimPrefix(proxy, "http://"), ":3128")
	tunnel := func(target string) string {
		return `(printf 'CONNECT ` + target + ` HTTP/1.1\r\nHost: ` + target + `\r\n\r\n'; sleep 4) | ` +
			`nc -w 6 ` + host + ` 3128 | head -1`
	}
	script := "sleep 5\nenv | grep -c '^HTTPS_PROXY=http://'\n" +
		"wget -q -T 5 -O /dev/null http://1.1.1.1 && echo DIRECT-OPEN\n" +
		"echo allowed: $( " + tunnel("example.com:443") + ")\n" +
		"echo denied: $( " + tunnel("github.com:443") + ")\necho done"
	st, out := runPod(t, viaProxy, core.WorkloadSpec{Name: "proxied", Network: core.NetworkEgress,
		Command: []string{"sh", "-c", script}, Trust: core.TrustCredentialed})
	if st.Phase != core.WorkloadSucceeded || strings.Contains(out, "DIRECT-OPEN") ||
		!strings.Contains(out, "allowed: HTTP/1.1 200") || !strings.Contains(out,
		"denied: HTTP/1.1 403") ||
		!strings.HasPrefix(out, "1") {
		t.Fatalf("status %+v:\n%s", st, out)
	}
}
