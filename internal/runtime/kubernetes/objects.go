package kubernetes

import (
	"context"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/ungarscool1/goblins/internal/core"
)

func secretName(workload string) string { return objectName(workload) + "-env" }

// allSecrets are a workload's secrets and test secrets, which reach it the same way.
func allSecrets(spec core.WorkloadSpec) map[string]core.Secret {
	out := maps.Clone(spec.Secrets)
	if out == nil {
		out = map[string]core.Secret{}
	}
	maps.Copy(out, spec.TestSecrets)
	return out
}

// createSecret stores a workload's secrets in a Secret of its own, which the pod refers to by
// key. Values are never part of the pod, where anyone allowed to read pods would see them.
func (r *Runtime) createSecret(ctx context.Context, spec core.WorkloadSpec,
	labels map[string]string) error {
	data := map[string]string{}
	for k, v := range allSecrets(spec) {
		data[k] = v.Reveal()
	}
	s := map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata":   meta{Name: secretName(spec.Name), Labels: labels},
		"stringData": data}
	err := r.c.call(ctx, http.MethodPost, r.ns("secrets"), s, nil)
	if core.IsKind(err, core.KindConflict) {
		return nil // the same attempt, retried
	}
	return err
}

// ownSecret makes the pod the Secret's owner, so the garbage collector removes it with the pod
// even if Cleanup never runs. Failing only delays that, so the error is dropped.
func (r *Runtime) ownSecret(ctx context.Context, workload, uid string) {
	if uid == "" {
		return
	}
	_ = r.c.patch(ctx, r.ns("secrets")+"/"+secretName(workload), map[string]any{
		"metadata": map[string]any{"ownerReferences": []ownerRef{{APIVersion: "v1", Kind: "Pod",
			Name: objectName(workload), UID: uid}}}})
}

// Network policies, one per network mode, selecting pods by their label. With no rule, a
// policy type denies everything; the egress one allows the internet and DNS only.
const (
	policyNone   = "goblins-workloads-no-network"
	policyEgress = "goblins-workloads-egress"
)

func (r *Runtime) policies() []map[string]any {
	selectorOf := func(mode core.NetworkPolicy) map[string]any {
		return map[string]any{"matchLabels": map[string]string{labelManaged: managedBy,
			labelNetwork: string(mode)}}
	}
	dns := map[string]any{"ports": []map[string]any{{"protocol": "UDP", "port": 53},
		{"protocol": "TCP", "port": 53}},
		"to": []map[string]any{{"namespaceSelector": map[string]any{},
			"podSelector": map[string]any{
				"matchLabels": map[string]string{"k8s-app": "kube-dns"}}}}}
	internet := map[string]any{"to": []map[string]any{{"ipBlock": map[string]any{
		"cidr": "0.0.0.0/0", "except": r.o.EgressExcept}}}}
	if r.o.EgressProxy != "" {
		port, _ := proxyPort(r.o.EgressProxy)
		internet = map[string]any{"ports": []map[string]any{{"protocol": "TCP", "port": port}},
			"to": []map[string]any{{"podSelector": map[string]any{"matchLabels": map[string]string{
				"app.kubernetes.io/component": "egress-proxy"}}}}}
	}
	mk := func(name string, mode core.NetworkPolicy, egress []map[string]any) map[string]any {
		return map[string]any{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
			"metadata": meta{Name: name, Labels: map[string]string{labelManaged: managedBy}},
			"spec": map[string]any{"podSelector": selectorOf(mode),
				"policyTypes": []string{"Ingress", "Egress"}, "ingress": []any{}, "egress": egress}}
	}
	return []map[string]any{mk(policyNone, core.NetworkNone, []map[string]any{}),
		mk(policyEgress, core.NetworkEgress, []map[string]any{dns, internet})}
}

// proxyPort is the port of an egress proxy URL, which the egress policy opens.
func proxyPort(proxy string) (int, error) {
	u, err := url.Parse(proxy)
	if err != nil || u.Scheme != "http" || u.Hostname() == "" {
		return 0, core.Errorf(core.KindInvalid, "kubernetes.config",
			"egress_proxy must be an http:// URL of the proxy")
	}
	if u.Port() == "" {
		return 80, nil
	}
	return strconv.Atoi(u.Port())
}

// ensurePolicies checks the policies exist, once per process, and creates those missing. The
// Helm chart creates them, so its role only lets Goblins read them; one that exists is left as
// it is, as an operator may have tightened it.
func (r *Runtime) ensurePolicies(ctx context.Context) error {
	r.mu.Lock()
	done := r.nets
	r.mu.Unlock()
	if done {
		return nil
	}
	base := "/apis/networking.k8s.io/v1/namespaces/" + url.PathEscape(r.o.Namespace) +
		"/networkpolicies"
	for _, p := range r.policies() {
		name := p["metadata"].(meta).Name
		err := r.c.call(ctx, http.MethodGet, base+"/"+name, nil, nil)
		if core.IsKind(err, core.KindNotFound) {
			err = r.c.call(ctx, http.MethodPost, base, p, nil)
		}
		if err != nil && !core.IsKind(err, core.KindConflict) {
			return err
		}
	}
	r.mu.Lock()
	r.nets = true
	r.mu.Unlock()
	return nil
}

// EnsureVolume implements core.ExecutionRuntime with a PersistentVolumeClaim.
func (r *Runtime) EnsureVolume(ctx context.Context, name string, labels map[string]string) error {
	l, original := kubeLabels(labels)
	spec := map[string]any{"accessModes": []string{"ReadWriteOnce"},
		"resources": map[string]any{"requests": map[string]string{"storage": r.o.VolumeSize}}}
	if r.o.StorageClass != "" {
		spec["storageClassName"] = r.o.StorageClass
	}
	pvc := map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": meta{Name: objectName(name), Labels: l,
			Annotations: map[string]string{annotationName: name, annotationLabels: original}},
		"spec": spec}
	err := r.c.call(ctx, http.MethodPost, r.ns("persistentvolumeclaims"), pvc, nil)
	if core.IsKind(err, core.KindConflict) {
		return nil
	}
	return err
}

// RemoveVolume implements core.ExecutionRuntime.
func (r *Runtime) RemoveVolume(ctx context.Context, name string) error {
	err := r.c.call(ctx, http.MethodDelete, r.ns("persistentvolumeclaims")+"/"+objectName(name),
		nil, nil)
	if core.IsKind(err, core.KindNotFound) {
		return nil
	}
	return err
}

// ListVolumes implements core.VolumeLister.
func (r *Runtime) ListVolumes(ctx context.Context,
	labels map[string]string) ([]core.VolumeStatus, error) {
	var out struct {
		Items []struct {
			Metadata meta `json:"metadata"`
		} `json:"items"`
	}
	q := url.Values{"labelSelector": {selector(labels)}}
	path := r.ns("persistentvolumeclaims") + "?" + q.Encode()
	if err := r.c.call(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	res := make([]core.VolumeStatus, 0, len(out.Items))
	for _, it := range out.Items {
		name := it.Metadata.Annotations[annotationName]
		if name == "" {
			name = it.Metadata.Name
		}
		res = append(res, core.VolumeStatus{Name: name,
			Labels: originalLabels(it.Metadata.Annotations, it.Metadata.Labels)})
	}
	return res, nil
}

// Logs implements core.ExecutionRuntime. The API server merges stdout and stderr, so both
// writers get the merged log unless split streams are enabled: nothing a caller looks for
// in either is lost.
func (r *Runtime) Logs(ctx context.Context, name string, follow bool, stdout,
	stderr io.Writer) error {
	if err := r.waitStarted(ctx, name); err != nil {
		return err
	}
	if !r.o.SplitStreams {
		return r.stream(ctx, name, follow, "", io.MultiWriter(stdout, stderr))
	}
	var wg sync.WaitGroup
	var errOut, errErr error
	wg.Add(2)
	go func() { defer wg.Done(); errOut = r.stream(ctx, name, follow, "Stdout", stdout) }()
	go func() { defer wg.Done(); errErr = r.stream(ctx, name, follow, "Stderr", stderr) }()
	wg.Wait()
	if errOut != nil {
		return errOut
	}
	return errErr
}

func (r *Runtime) stream(ctx context.Context, name string, follow bool, which string,
	w io.Writer) error {
	q := url.Values{"container": {container}}
	if follow {
		q.Set("follow", "true")
	}
	if which != "" {
		q.Set("stream", which)
	}
	req, err := r.c.request(ctx, http.MethodGet, r.ns("pods")+"/"+objectName(name)+"/log?"+
		q.Encode(), "", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/plain")
	resp, err := r.c.stream.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return core.Wrap(core.KindCancelled, "kubernetes.logs", ctx.Err())
		}
		return core.Wrap(core.KindTransient, "kubernetes.logs", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return classify("logs "+name, resp.StatusCode, data)
	}
	if _, err := io.Copy(w, resp.Body); err != nil && ctx.Err() == nil &&
		!strings.Contains(err.Error(), "closed") {
		return core.Wrap(core.KindTransient, "kubernetes.logs", err)
	}
	return nil
}
