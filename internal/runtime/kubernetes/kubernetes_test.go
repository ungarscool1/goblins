package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers/contract"
	goruntime "github.com/ungarscool1/goblins/internal/runtime"
	"github.com/ungarscool1/goblins/internal/runtime/kubernetes/fakek8s"
)

const token = "sa-token-value"

// behavior follows the contract's convention: ok prints, fail exits 3, hang runs.
func behavior(p fakek8s.Object) fakek8s.Result {
	c := p["spec"].(fakek8s.Object)["containers"].([]any)[0].(fakek8s.Object)
	cmd := c["command"].([]any)
	switch cmd[0] {
	case "ok":
		out := ""
		if len(cmd) > 1 {
			out = cmd[1].(string) + "\n"
		}
		return fakek8s.Result{Stdout: out, Stderr: "to stderr\n"}
	case "fail":
		return fakek8s.Result{Exit: 3}
	case "hang":
		return fakek8s.Result{Hang: true}
	case "pull":
		return fakek8s.Result{Waiting: "ImagePullBackOff"}
	}
	return fakek8s.Result{Exit: 127}
}

func newRuntime(t *testing.T, mod func(*Options)) (*Runtime, *fakek8s.Server) {
	t.Helper()
	srv := fakek8s.New("goblins-work", token)
	srv.Behavior = behavior
	t.Cleanup(srv.Close)
	o := Options{Client: ClientOptions{URL: srv.URL, Token: core.NewSecret(token)},
		Namespace: "goblins-work", NetworkPolicies: true, DiskLimits: true,
		Defaults: core.Resources{MemoryBytes: 1 << 30, CPUs: 1}}
	if mod != nil {
		mod(&o)
	}
	rt, err := New("k8s", o)
	if err != nil {
		t.Fatal(err)
	}
	return rt, srv
}

func TestRuntimeContract(t *testing.T) {
	contract.ExecutionRuntime(t, func(t *testing.T) contract.RuntimeHarness {
		rt, _ := newRuntime(t, nil)
		return contract.RuntimeHarness{Runtime: rt, Image: "busybox:1"}
	})
}

func pods(t *testing.T, srv *fakek8s.Server) []fakek8s.Object {
	t.Helper()
	var out []fakek8s.Object
	for _, p := range srv.Objects("pods") {
		out = append(out, p)
	}
	return out
}

// Every pod is confined the same way, whatever asked for it.
func TestPodsAreConfined(t *testing.T) {
	rt, srv := newRuntime(t, nil)
	spec := core.WorkloadSpec{Name: "goblins-J1_Plan-agent-1", Image: "agent:1",
		Command: []string{"ok"}, Network: core.NetworkEgress, Timeout: 90 * time.Second,
		Env:      map[string]string{"HOME": "/home/agent"},
		Secrets:  map[string]core.Secret{"CLAUDE_CODE_OAUTH_TOKEN": core.NewSecret("oauth-secret")},
		Mounts:   []core.Mount{{Volume: "goblins-personal-t1-impl", Target: "/workspace"}},
		Labels:   map[string]string{"goblins.task": "t1", "goblins.design": "rev 2/hash"},
		Security: core.SecurityProfile{ReadOnlyRoot: true}, Trust: core.TrustCredentialed}
	if _, err := rt.Create(context.Background(), spec); err != nil {
		t.Fatalf("%v (refused: %v)", err, srv.Refused)
	}
	all := pods(t, srv)
	if len(all) != 1 {
		t.Fatalf("pods = %d", len(all))
	}
	raw, _ := json.Marshal(all[0])
	text := string(raw)
	if strings.Contains(text, "oauth-secret") {
		t.Fatal("a secret value is in the pod")
	}
	for _, want := range []string{`"automountServiceAccountToken":false`,
		`"enableServiceLinks":false`,
		`"runAsNonRoot":true`, `"readOnlyRootFilesystem":true`, `"drop":["ALL"]`,
		`"activeDeadlineSeconds":90`, `"secretKeyRef":{"key":`, `"restartPolicy":"Never"`,
		`"claimName":"goblins-personal-t1-impl"`, `"goblins.dev/network":"egress"`} {
		if !strings.Contains(text, want) {
			t.Errorf("pod lacks %s:\n%s", want, text)
		}
	}
	secrets := srv.Objects("secrets")
	if len(secrets) != 1 {
		t.Fatalf("secrets = %d", len(secrets))
	}
	for _, s := range secrets {
		if s["data"].(fakek8s.Object)["CLAUDE_CODE_OAUTH_TOKEN"] != "oauth-secret" {
			t.Fatalf("secret = %v", s)
		}
		refs := s["metadata"].(fakek8s.Object)["ownerReferences"]
		if refs == nil {
			t.Fatal("the secret is not owned by its pod")
		}
	}
	// Labels that are not valid label values still select, and come back as they were.
	list, err := rt.List(context.Background(), map[string]string{"goblins.design": "rev 2/hash"})
	if err != nil || len(list) != 1 || list[0].Labels["goblins.design"] != "rev 2/hash" ||
		list[0].Name != spec.Name {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := rt.Cleanup(context.Background(), spec.Name); err != nil {
		t.Fatal(err)
	}
	if len(srv.Objects("secrets")) != 0 || len(srv.Objects("pods")) != 0 {
		t.Fatal("cleanup left objects behind")
	}
}

func TestNetworkIsolationNeedsPolicies(t *testing.T) {
	rt, srv := newRuntime(t, func(o *Options) { o.NetworkPolicies = false })
	_, err := rt.Create(context.Background(), core.WorkloadSpec{Name: "w", Image: "i",
		Command: []string{"ok"}, Network: core.NetworkNone})
	if !core.IsKind(err, core.KindUnsupported) || len(pods(t, srv)) != 0 {
		t.Fatalf("no-network workload without policies: %v", err)
	}
	rt, srv = newRuntime(t, nil)
	if _, err := rt.Create(context.Background(), core.WorkloadSpec{Name: "w", Image: "i",
		Command: []string{"ok"}, Network: core.NetworkNone}); err != nil {
		t.Fatal(err)
	}
	pol := srv.Objects("networkpolicies")
	none, egress := pol[policyNone], pol[policyEgress]
	if none == nil || egress == nil {
		t.Fatalf("policies = %v", pol)
	}
	raw, _ := json.Marshal(egress)
	if !strings.Contains(string(raw), `"169.254.0.0/16"`) || !strings.Contains(string(raw),
		`"ingress":[]`) {
		t.Fatalf("egress policy = %s", raw)
	}
	raw, _ = json.Marshal(none)
	if !strings.Contains(string(raw), `"egress":[]`) {
		t.Fatalf("no-network policy = %s", raw)
	}
}

func TestDevicesAreOnlyOfferedWhenConfigured(t *testing.T) {
	rt, _ := newRuntime(t, nil)
	for _, c := range []core.Capability{core.CapRuntimeGPU, core.CapRuntimeKVM,
		core.CapRuntimePrivileged} {
		if rt.Info().Has(c) {
			t.Errorf("%s offered by default", c)
		}
	}
	rt, srv := newRuntime(t, func(o *Options) { o.KVMResource = "devices.kubevirt.io/kvm" })
	if _, err := rt.Create(context.Background(), core.WorkloadSpec{Name: "vm", Image: "i",
		Command: []string{"ok"}, Network: core.NetworkNone,
		Security: core.SecurityProfile{KVM: true}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(pods(t, srv)[0])
	if !strings.Contains(string(raw), `"devices.kubevirt.io/kvm":"1"`) {
		t.Fatalf("pod = %s", raw)
	}
}

func run(t *testing.T, rt *Runtime, spec core.WorkloadSpec) core.WorkloadStatus {
	t.Helper()
	ctx := context.Background()
	if _, err := rt.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := rt.Status(ctx, spec.Name)
		if err != nil {
			t.Fatal(err)
		}
		if st.Phase.Terminal() {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("never finished")
	return core.WorkloadStatus{}
}

func TestTimeoutsAndStartFailures(t *testing.T) {
	rt, _ := newRuntime(t, nil)
	st := run(t, rt, core.WorkloadSpec{Name: "slow", Image: "i", Command: []string{"hang"},
		Network: core.NetworkNone, Timeout: time.Second})
	if st.Phase != core.WorkloadTimedOut {
		t.Fatalf("timed out workload = %+v", st)
	}
	st = run(t, rt, core.WorkloadSpec{Name: "bad-image", Image: "nope", Command: []string{"pull"},
		Network: core.NetworkNone})
	if st.Phase != core.WorkloadFailed {
		t.Fatalf("image pull failure = %+v", st)
	}
}

func TestLogs(t *testing.T) {
	for _, split := range []bool{false, true} {
		rt, _ := newRuntime(t, func(o *Options) { o.SplitStreams = split })
		spec := core.WorkloadSpec{Name: "talk", Image: "i", Command: []string{"ok", "out"},
			Network: core.NetworkNone}
		if _, err := rt.Create(context.Background(), spec); err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		if err := rt.Logs(context.Background(), spec.Name, true, &out, &errb); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "out") || !strings.Contains(errb.String(), "to stderr") {
			t.Fatalf("split=%v: stdout %q, stderr %q", split, out.String(), errb.String())
		}
		if split && (strings.Contains(out.String(), "stderr") || strings.Contains(errb.String(),
			"out\n")) {
			t.Fatalf("streams were mixed: %q / %q", out.String(), errb.String())
		}
		// Joined, a merged log is there once: a caller parsing it, such as the JSON of an
		// agent's login status, reads one document.
		if got := goruntime.Combined(out.String(), errb.String()); strings.Count(got, "to stderr") != 1 {
			t.Fatalf("split=%v: joined output %q", split, got)
		}
	}
}

func TestErrorsNameTheRole(t *testing.T) {
	srv := fakek8s.New("goblins-work", token)
	defer srv.Close()
	rt, _ := New("k8s", Options{Client: ClientOptions{URL: srv.URL, Token: core.NewSecret(token)},
		Namespace: "kube-system"})
	_, err := rt.Status(context.Background(), "x")
	if !core.IsKind(err, core.KindForbidden) || !strings.Contains(err.Error(), "service account") {
		t.Fatalf("err = %v", err)
	}
	rt, _ = New("k8s", Options{Client: ClientOptions{URL: srv.URL, Token: core.NewSecret("wrong")},
		Namespace: "goblins-work"})
	if _, err := rt.Status(context.Background(), "x"); !core.IsKind(err, core.KindAuthRequired) {
		t.Fatalf("bad token: %v", err)
	}
}

func TestObjectNames(t *testing.T) {
	for in, ok := range map[string]bool{"goblins-j1-plan-1": true, "Goblins_J1": false,
		strings.Repeat("a", 70): false} {
		got := objectName(in)
		if (got == in) != ok || len(got) > 63 || !dnsName.MatchString(got) {
			t.Errorf("objectName(%q) = %q", in, got)
		}
	}
	if objectName("a_b") == objectName("a-b") {
		t.Error("two names map to one object")
	}
}

func TestConfiguration(t *testing.T) {
	if _, err := New("k", Options{Client: ClientOptions{URL: "http://cluster.example:6443",
		Token: core.NewSecret("t")}, Namespace: "n"}); err == nil {
		t.Error("plain http to a remote API server was accepted")
	}
	if _, err := New("k", Options{Client: ClientOptions{URL: "https://c:6443"},
		Namespace: "n"}); err == nil {
		t.Error("no token was accepted")
	}
	if _, err := New("k", Options{Client: ClientOptions{URL: "https://c:6443",
		Token: core.NewSecret("t")}}); err == nil {
		t.Error("no namespace was accepted")
	}
	if _, err := New("k", Options{Client: ClientOptions{Getenv: func(string) (string, bool) {
		return "", false
	}}, Namespace: "n"}); err == nil {
		t.Error("in-cluster defaults outside a cluster were accepted")
	}
}

// A pod with no network waits behind a guard until its policy is in force; one with egress
// does not need one.
func TestNoNetworkPodsAreGuarded(t *testing.T) {
	rt, srv := newRuntime(t, nil)
	for name, n := range map[string]core.NetworkPolicy{"none": core.NetworkNone,
		"egress": core.NetworkEgress} {
		if _, err := rt.Create(context.Background(), core.WorkloadSpec{Name: name, Image: "i",
			Command: []string{"ok"}, Network: n}); err != nil {
			t.Fatal(err)
		}
	}
	for name, p := range srv.Objects("pods") {
		raw, _ := json.Marshal(p)
		guarded := strings.Contains(string(raw), `"name":"network-guard"`)
		if guarded != (name == "none") {
			t.Errorf("%s: guarded = %v", name, guarded)
		}
	}
}
