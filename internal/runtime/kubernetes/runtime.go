package kubernetes

import (
	"context"
	"maps"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	rt "github.com/ungarscool1/goblins/internal/runtime"
)

const (
	typeName      = "kubernetes"
	container     = "main"
	defaultUID    = 10001
	tmpVolume     = "tmp"
	tmpMount      = "/tmp"
	defaultTmp    = "1Gi"
	defaultVolume = "10Gi"
)

// Options configures the runtime.
type Options struct {
	Client    ClientOptions
	Namespace string
	// ServiceAccount runs workloads. It should have no role bound: workloads never get a
	// token for it anyway.
	ServiceAccount string
	UID            int64
	// NetworkPolicies says the cluster enforces NetworkPolicy objects. Without it, isolation
	// from the network cannot be promised, so workloads asking for none are refused.
	NetworkPolicies bool
	// EgressExcept are destinations egress workloads may not reach, by default the cluster's
	// private ranges and the cloud metadata address.
	EgressExcept []string
	StorageClass string
	VolumeSize   string
	TmpSize      string
	PullPolicy   string
	DiskLimits   bool
	// GPUResource and KVMResource name the extended resources a device plugin offers, such as
	// nvidia.com/gpu or devices.kubevirt.io/kvm. Without them, those devices are refused.
	GPUResource string
	KVMResource string
	// SplitStreams asks the API server for stdout and stderr apart, which needs the
	// PodLogsQuerySplitStreams feature. Without it, both writers get the merged log.
	SplitStreams bool
	Defaults     core.Resources
	// EgressProxy, when set, is the only way out for egress pods: their policy allows DNS and
	// the proxy's pods, and nothing else.
	EgressProxy string
}

// Runtime implements core.ExecutionRuntime on Kubernetes.
type Runtime struct {
	name string
	o    Options
	c    *client
	mu   sync.Mutex
	nets bool
}

// New returns a runtime.
func New(name string, o Options) (*Runtime, error) {
	c, err := newClient(o.Client)
	if err != nil {
		return nil, err
	}
	if o.Namespace == "" {
		return nil, core.Errorf(core.KindInvalid, "kubernetes.config", "namespace is required")
	}
	if o.UID == 0 {
		o.UID = defaultUID
	}
	if o.UID < 0 {
		return nil, core.Errorf(core.KindInvalid, "kubernetes.config", "workers cannot run as root")
	}
	if o.EgressProxy != "" {
		if _, err := proxyPort(o.EgressProxy); err != nil {
			return nil, err
		}
	}
	if len(o.EgressExcept) == 0 {
		o.EgressExcept = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
			"169.254.0.0/16", "100.64.0.0/10"}
	}
	for k, def := range map[*string]string{&o.VolumeSize: defaultVolume, &o.TmpSize: defaultTmp,
		&o.PullPolicy: "IfNotPresent"} {
		if *k == "" {
			*k = def
		}
	}
	return &Runtime{name: name, o: o, c: c}, nil
}

// Info implements core.Provider. Privileged pods are never offered.
func (r *Runtime) Info() core.ProviderInfo {
	caps := []core.Capability{core.CapRuntimeVolumes, core.CapRuntimeVolumeList,
		core.CapRuntimeNetworkEgress, core.CapRuntimeResourceLimits, core.CapRuntimeReadOnlyRoot}
	if r.o.NetworkPolicies {
		caps = append(caps, core.CapRuntimeNetworkNone)
	}
	if r.o.DiskLimits {
		caps = append(caps, core.CapRuntimeDiskLimits)
	}
	if r.o.GPUResource != "" {
		caps = append(caps, core.CapRuntimeGPU)
	}
	if r.o.KVMResource != "" {
		caps = append(caps, core.CapRuntimeKVM)
	}
	return core.ProviderInfo{Kind: core.ProviderRuntime, Type: typeName, Name: r.name,
		ContractVersion: core.ContractVersion, Capabilities: caps}
}

func (r *Runtime) ns(kind string) string {
	return "/api/v1/namespaces/" + url.PathEscape(r.o.Namespace) + "/" + kind
}

// Create implements core.ExecutionRuntime.
func (r *Runtime) Create(ctx context.Context, spec core.WorkloadSpec) (core.WorkloadStatus, error) {
	if err := core.CheckWorkloadSpec(r, spec); err != nil {
		return core.WorkloadStatus{}, err
	}
	p, err := r.buildPod(spec)
	if err != nil {
		return core.WorkloadStatus{}, err
	}
	if r.o.NetworkPolicies {
		if err := r.ensurePolicies(ctx); err != nil {
			return core.WorkloadStatus{}, err
		}
	}
	// A mounted volume that does not exist yet is created, as Docker does: a pod waiting for
	// a claim nobody makes would never start.
	for _, m := range spec.Mounts {
		if err := r.EnsureVolume(ctx, m.Volume, nil); err != nil {
			return core.WorkloadStatus{}, err
		}
	}
	if len(spec.Secrets) > 0 {
		if err := r.createSecret(ctx, spec, p.Metadata.Labels); err != nil {
			return core.WorkloadStatus{}, err
		}
	}
	var created pod
	err = r.c.call(ctx, http.MethodPost, r.ns("pods"), p, &created)
	if core.IsKind(err, core.KindConflict) {
		return r.Status(ctx, spec.Name)
	}
	if err != nil {
		return core.WorkloadStatus{}, err
	}
	if len(spec.Secrets) > 0 {
		r.ownSecret(ctx, spec.Name, created.Metadata.UID)
	}
	return r.status(created), nil
}

// Status implements core.ExecutionRuntime.
func (r *Runtime) Status(ctx context.Context, name string) (core.WorkloadStatus, error) {
	var p pod
	if err := r.c.call(ctx, http.MethodGet, r.ns("pods")+"/"+objectName(name), nil,
		&p); err != nil {
		return core.WorkloadStatus{}, err
	}
	return r.status(p), nil
}

// Cancel implements core.ExecutionRuntime. The pod is given a deadline that has passed, so
// the kubelet stops it, and an annotation, so it reads as cancelled rather than timed out.
func (r *Runtime) Cancel(ctx context.Context, name string) error {
	st, err := r.Status(ctx, name)
	if core.IsKind(err, core.KindNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.Phase.Terminal() {
		return nil
	}
	return r.c.patch(ctx, r.ns("pods")+"/"+objectName(name), map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{annotationCancelled: "true"}},
		"spec":     map[string]any{"activeDeadlineSeconds": 1},
	})
}

// Cleanup implements core.ExecutionRuntime.
func (r *Runtime) Cleanup(ctx context.Context, name string) error {
	q := "?gracePeriodSeconds=0&propagationPolicy=Background"
	err := r.c.call(ctx, http.MethodDelete, r.ns("pods")+"/"+objectName(name)+q, nil, nil)
	if err != nil && !core.IsKind(err, core.KindNotFound) {
		return err
	}
	err = r.c.call(ctx, http.MethodDelete, r.ns("secrets")+"/"+secretName(name), nil, nil)
	if err != nil && !core.IsKind(err, core.KindNotFound) {
		return err
	}
	return nil
}

// List implements core.ExecutionRuntime.
func (r *Runtime) List(ctx context.Context, labels map[string]string) ([]core.WorkloadStatus,
	error) {
	var out struct {
		Items []pod `json:"items"`
	}
	q := url.Values{"labelSelector": {selector(labels)}}
	if err := r.c.call(ctx, http.MethodGet, r.ns("pods")+"?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	res := make([]core.WorkloadStatus, 0, len(out.Items))
	for _, p := range out.Items {
		res = append(res, r.status(p))
	}
	return res, nil
}

// Containers that cannot start never will without a person, so they end the workload.
var fatalWaiting = []string{"ErrImagePull", "ImagePullBackOff", "InvalidImageName",
	"CreateContainerConfigError", "CreateContainerError"}

func (r *Runtime) status(p pod) core.WorkloadStatus {
	st := core.WorkloadStatus{Name: p.Metadata.Annotations[annotationName],
		Labels: originalLabels(p.Metadata.Annotations, p.Metadata.Labels)}
	if st.Name == "" {
		st.Name = p.Metadata.Name
	}
	var cs *containerStatus
	for i := range p.Status.ContainerStatuses {
		if p.Status.ContainerStatuses[i].Name == container {
			cs = &p.Status.ContainerStatuses[i]
		}
	}
	exit := 1
	if cs != nil && cs.State.Terminated != nil {
		exit = cs.State.Terminated.ExitCode
	}
	cancelled := p.Metadata.Annotations[annotationCancelled] == "true"
	switch p.Status.Phase {
	case "Running":
		st.Phase = core.WorkloadRunning
	case "Succeeded":
		st.Phase = core.WorkloadSucceeded
	case "Failed":
		st.Phase, st.ExitCode = core.WorkloadFailed, max(exit, 1)
		if p.Status.Reason == "DeadlineExceeded" {
			st.Phase = core.WorkloadTimedOut
		}
		if cancelled {
			st.Phase = core.WorkloadCancelled
		}
	default:
		st.Phase = core.WorkloadPending
		if cs != nil && cs.State.Waiting != nil && slices.Contains(fatalWaiting,
			cs.State.Waiting.Reason) {
			st.Phase, st.ExitCode = core.WorkloadFailed, 125
		}
	}
	return st
}

// buildPod is pure, so tests can check the security posture of every pod without a cluster.
func (r *Runtime) buildPod(spec core.WorkloadSpec) (pod, error) {
	if spec.Image == "" || len(spec.Command) == 0 {
		return pod{}, core.Errorf(core.KindInvalid, "kubernetes.create",
			"workload needs an image and a command")
	}
	labels, original := kubeLabels(spec.Labels)
	labels[labelNetwork] = string(spec.Network)
	p := pod{APIVersion: "v1", Kind: "Pod", Metadata: meta{Name: objectName(spec.Name),
		Labels: labels, Annotations: map[string]string{annotationName: spec.Name,
			annotationLabels: original}}}
	s := &p.Spec
	f := false
	s.RestartPolicy, s.AutomountToken, s.EnableServiceLinks = "Never", &f, &f
	s.ServiceAccountName = r.o.ServiceAccount
	grace := int64(5)
	s.TerminationGrace = &grace
	if spec.Timeout > 0 {
		secs := max(int64(spec.Timeout/time.Second), 1)
		s.ActiveDeadline = &secs
	}
	t := true
	uid := r.o.UID
	s.SecurityContext = podSecurity{RunAsNonRoot: &t, RunAsUser: &uid, RunAsGroup: &uid,
		FSGroup: &uid, FSGroupChangePolicy: "OnRootMismatch",
		Seccomp: &seccomp{Type: "RuntimeDefault"}}
	c := containerSpec{Name: container, Image: spec.Image, Command: spec.Command,
		WorkingDir: spec.WorkDir, ImagePullPolicy: r.o.PullPolicy,
		SecurityContext: containerSecurity{AllowPrivilegeEscalation: &f, Privileged: &f,
			ReadOnlyRoot: spec.Security.ReadOnlyRoot, Capabilities: dropAll}}
	for _, k := range sortedKeys(spec.Env) {
		c.Env = append(c.Env, envVar{Name: k, Value: spec.Env[k]})
	}
	if spec.Network == core.NetworkEgress {
		for _, kv := range rt.ProxyEnv(r.o.EgressProxy) {
			k, v, _ := strings.Cut(kv, "=")
			c.Env = append(c.Env, envVar{Name: k, Value: v})
		}
	}
	for _, k := range slices.Sorted(maps.Keys(spec.Secrets)) {
		c.Env = append(c.Env, envVar{Name: k, ValueFrom: &envSource{SecretKeyRef: &keyRef{
			Name: secretName(spec.Name), Key: k}}})
	}
	c.Resources = r.resources(spec)
	s.Volumes = append(s.Volumes, volume{Name: tmpVolume,
		EmptyDir: &emptyDir{SizeLimit: r.o.TmpSize}})
	c.VolumeMounts = append(c.VolumeMounts, volumeMount{Name: tmpVolume, MountPath: tmpMount})
	for i, m := range spec.Mounts {
		if m.Volume == "" || !path.IsAbs(m.Target) || path.Clean(m.Target) != m.Target {
			return pod{}, core.Errorf(core.KindInvalid, "kubernetes.create",
				"mount %d needs a volume name and a clean absolute target", i)
		}
		name := "v" + strconv.Itoa(i)
		s.Volumes = append(s.Volumes, volume{Name: name,
			PVC: &pvcSource{ClaimName: objectName(m.Volume),
				ReadOnly: m.ReadOnly}})
		c.VolumeMounts = append(c.VolumeMounts, volumeMount{Name: name, MountPath: m.Target,
			ReadOnly: m.ReadOnly})
	}
	s.Containers = []containerSpec{c}
	return p, nil
}

var dropAll = &capabilities{Drop: []string{"ALL"}}

func (r *Runtime) resources(spec core.WorkloadSpec) *resources {
	res := spec.Resources
	if res.MemoryBytes == 0 {
		res.MemoryBytes = r.o.Defaults.MemoryBytes
	}
	if res.CPUs == 0 {
		res.CPUs = r.o.Defaults.CPUs
	}
	out := &resources{Limits: map[string]string{}, Requests: map[string]string{}}
	if res.MemoryBytes > 0 {
		mem := strconv.FormatInt(res.MemoryBytes, 10)
		out.Limits["memory"], out.Requests["memory"] = mem, mem
	}
	if res.CPUs > 0 {
		out.Limits["cpu"] = strconv.FormatInt(int64(res.CPUs*1000), 10) + "m"
		out.Requests["cpu"] = strconv.FormatInt(max(int64(res.CPUs*250), 50), 10) + "m"
	}
	if res.DiskBytes > 0 {
		out.Limits["ephemeral-storage"] = strconv.FormatInt(res.DiskBytes, 10)
	}
	if spec.Security.GPU {
		out.Limits[r.o.GPUResource] = "1"
	}
	if spec.Security.KVM {
		out.Limits[r.o.KVMResource] = "1"
	}
	return out
}

// waitStarted waits until the container has run or failed to start. The log of a container
// that has not started yet is an error, not an empty stream.
func (r *Runtime) waitStarted(ctx context.Context, name string) error {
	for {
		var p pod
		if err := r.c.call(ctx, http.MethodGet, r.ns("pods")+"/"+objectName(name), nil,
			&p); err != nil {
			return err
		}
		if st := r.status(p); st.Phase != core.WorkloadPending || started(p) {
			return nil
		}
		select {
		case <-ctx.Done():
			return core.Wrap(core.KindCancelled, "kubernetes.logs", ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func started(p pod) bool {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Running != nil || cs.State.Terminated != nil {
			return true
		}
	}
	return false
}
