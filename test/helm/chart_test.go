// Package helm renders the Helm chart and checks what it would create. It needs the helm
// binary, and is skipped without it.
package helm

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type object struct {
	Kind     string         `yaml:"kind"`
	Metadata map[string]any `yaml:"metadata"`
	Spec     map[string]any `yaml:"spec"`
	Rules    []struct {
		APIGroups []string `yaml:"apiGroups"`
		Resources []string `yaml:"resources"`
		Verbs     []string `yaml:"verbs"`
	} `yaml:"rules"`
	Data map[string]string `yaml:"data"`
	Raw  string            `yaml:"-"`
}

func render(t *testing.T, args ...string) []object {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	cmd := exec.Command("helm", append([]string{"template", "g", "../../deploy/helm/goblins",
		"--namespace", "goblins"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, stderr.String())
	}
	var objs []object
	dec := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var node yaml.Node
		if err := dec.Decode(&node); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		var o object
		if err := node.Decode(&o); err != nil || o.Kind == "" {
			continue
		}
		raw, _ := yaml.Marshal(&node)
		o.Raw = string(raw)
		objs = append(objs, o)
	}
	return objs
}

func TestChartIsLeastPrivilege(t *testing.T) {
	objs := render(t, "--set", "agentLogin.enabled=true,metrics.enabled=true")
	kinds := map[string]int{}
	for _, o := range objs {
		kinds[o.Kind]++
		switch o.Kind {
		case "ClusterRole", "ClusterRoleBinding":
			t.Errorf("the chart asks for cluster-wide access: %s", o.Metadata["name"])
		case "Role":
			if o.Metadata["namespace"] != "goblins-workloads" {
				t.Errorf("role outside the workloads namespace: %v", o.Metadata)
			}
			for _, r := range o.Rules {
				for _, v := range r.Verbs {
					if v == "*" || v == "escalate" || v == "bind" || v == "impersonate" {
						t.Errorf("verb %s on %v", v, r.Resources)
					}
				}
				for _, res := range r.Resources {
					if res == "pods/exec" || res == "pods/attach" || res == "*" {
						t.Errorf("resource %s granted", res)
					}
				}
			}
		case "Namespace":
			labels := o.Metadata["labels"].(map[string]any)
			if labels["pod-security.kubernetes.io/enforce"] != "restricted" {
				t.Errorf("workloads namespace is not restricted: %v", labels)
			}
		case "ServiceAccount":
			if o.Metadata["name"] == "goblins-workload" && !strings.Contains(o.Raw,
				"automountServiceAccountToken: false") {
				t.Error("the workload service account mounts a token")
			}
		case "Deployment", "Pod":
			for _, want := range []string{"runAsNonRoot: true", "allowPrivilegeEscalation: false",
				"readOnlyRootFilesystem: true", "RuntimeDefault"} {
				if !strings.Contains(o.Raw, want) {
					t.Errorf("%s lacks %s", o.Kind, want)
				}
			}
		}
	}
	if kinds["NetworkPolicy"] != 3 || kinds["Role"] != 1 || kinds["Deployment"] != 1 {
		t.Fatalf("objects = %v", kinds)
	}
}

func TestChartConfigReachesTheRuntime(t *testing.T) {
	for _, o := range render(t) {
		if o.Kind == "ConfigMap" {
			cfg := o.Data["goblins.yaml"]
			if !strings.Contains(cfg, "namespace: goblins-workloads") ||
				!strings.Contains(cfg, "network_policies: true") {
				t.Fatalf("config:\n%s", cfg)
			}
			return
		}
	}
	t.Fatal("no config")
}

func TestChartRefusesUnsafeShapes(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	for _, args := range [][]string{{"--set", "replicas=2"},
		{"--set", "storage.driver=postgres"}} {
		cmd := exec.Command("helm", append([]string{"template", "g", "../../deploy/helm/goblins"},
			args...)...)
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Errorf("%v rendered:\n%.300s", args, out)
		}
	}
}
