package kubernetes

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Annotations keep what Kubernetes names cannot hold as they are.
const (
	annotationName      = "goblins.dev/name"
	annotationLabels    = "goblins.dev/labels"
	annotationCancelled = "goblins.dev/cancelled"
	labelManaged        = "app.kubernetes.io/managed-by"
	labelNetwork        = "goblins.dev/network"
	managedBy           = "goblins"
)

var (
	dnsName    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	labelValue = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)?$`)
	notDNS     = regexp.MustCompile(`[^a-z0-9-]+`)
)

// objectName maps a workload or volume name onto a valid object name. A name that is valid
// already is kept, so objects are easy to find with kubectl; any other gets a hash of the
// original, so two names never map to one object.
func objectName(name string) string {
	if len(name) <= 63 && dnsName.MatchString(name) {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	base := strings.Trim(notDNS.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(base) > 50 {
		base = strings.TrimRight(base[:50], "-")
	}
	if base == "" {
		base = "goblins"
	}
	return base + "-" + hex.EncodeToString(sum[:])[:12]
}

// kubeLabels maps labels onto valid label values, and keeps the originals in an annotation.
// Selection uses the mapped values, which are deterministic.
func kubeLabels(l map[string]string) (map[string]string, string) {
	out := map[string]string{labelManaged: managedBy}
	for k, v := range l {
		out[k] = labelOf(v)
	}
	raw, _ := json.Marshal(l)
	return out, string(raw)
}

func labelOf(v string) string {
	if len(v) <= 63 && labelValue.MatchString(v) {
		return v
	}
	sum := sha256.Sum256([]byte(v))
	return "h-" + hex.EncodeToString(sum[:])[:40]
}

func originalLabels(annotations map[string]string, fallback map[string]string) map[string]string {
	var l map[string]string
	if json.Unmarshal([]byte(annotations[annotationLabels]), &l) == nil && l != nil {
		return l
	}
	l = maps.Clone(fallback)
	delete(l, labelManaged)
	delete(l, labelNetwork)
	return l
}

// selector is a label selector matching all of the labels.
func selector(l map[string]string) string {
	parts := []string{labelManaged + "=" + managedBy}
	for _, k := range sortedKeys(l) {
		parts = append(parts, k+"="+labelOf(l[k]))
	}
	return strings.Join(parts, ",")
}

func sortedKeys(m map[string]string) []string { return slices.Sorted(maps.Keys(m)) }
