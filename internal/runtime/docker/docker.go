// Package docker implements core.ExecutionRuntime on the Docker Engine HTTP API.
// Using the API directly keeps the dependency tree small and needs no Docker CLI
// in the orchestrator image. The engine may be remote: nothing assumes co-location.
//
// Every workload is confined by construction: non-root, all capabilities dropped,
// no new privileges, and only named volumes can be mounted, which makes it impossible
// to bind a host path such as the Docker socket into a worker.
package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
)

const (
	typeName       = "docker"
	labelDeadline  = "goblins.deadline"
	defaultUser    = "10001:10001"
	defaultNetwork = "goblins-workers"
	// exec because Docker otherwise defaults a tmpfs to noexec, and test runners such as
	// `go test` build a binary in /tmp and then run it. The writable workspace can already
	// hold executables, so noexec here would stop nothing.
	tmpfsOptions = "rw,exec,nosuid,nodev,size=1g"
)

// volumeName only admits Docker volume names, so a host path can never be mounted.
var volumeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// Options configures the runtime.
type Options struct {
	Host string // unix:///path or tcp://host:port
	// Network is the bridge used by egress workloads. It is created with
	// inter-container traffic disabled, and is separate from the control plane's.
	Network string
	User    string
	// PullMissing pulls images the engine does not have.
	PullMissing bool
	// DiskLimits declares that the storage driver supports per-container size limits.
	DiskLimits bool
	Defaults   core.Resources
}

// Runtime implements core.ExecutionRuntime.
type Runtime struct {
	name   string
	opts   Options
	http   *http.Client // no overall timeout: log streams are long lived
	base   string
	mu     sync.Mutex
	cancel map[string]bool
	netOK  bool
}

// New returns a runtime.
func New(name string, o Options) (*Runtime, error) {
	if o.Host == "" {
		o.Host = "unix:///var/run/docker.sock"
	}
	if o.Network == "" {
		o.Network = defaultNetwork
	}
	if o.User == "" {
		o.User = defaultUser
	}
	if o.User == "0" || o.User == "root" || strings.HasPrefix(o.User, "0:") {
		return nil, core.Errorf(core.KindInvalid, "docker.config", "workers cannot run as root")
	}
	u, err := url.Parse(o.Host)
	if err != nil {
		return nil, core.Errorf(core.KindInvalid, "docker.config", "invalid host")
	}
	r := &Runtime{name: name, opts: o, cancel: map[string]bool{}}
	transport := &http.Transport{MaxIdleConns: 8, IdleConnTimeout: 30 * time.Second}
	switch u.Scheme {
	case "unix":
		path := u.Path
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}
		r.base = "http://docker"
	case "tcp", "http":
		r.base = "http://" + u.Host
	default:
		return nil, core.Errorf(core.KindInvalid, "docker.config", "unsupported host scheme %q", u.Scheme)
	}
	r.http = &http.Client{Transport: transport}
	return r, nil
}

// Register adds the runtime to a registry.
func Register(reg *providers.Registry) {
	reg.Register(providers.KindRuntime, typeName, func(name string, deps providers.Deps, opts providers.Options) (any, error) {
		host := opts.String("host")
		if host == "" && deps.Getenv != nil {
			host, _ = deps.Getenv("DOCKER_HOST")
		}
		return New(name, Options{
			Host: host, Network: opts.String("network"), User: opts.String("user"),
			PullMissing: opts.Bool("pull_missing", true), DiskLimits: opts.Bool("disk_limits", false),
			Defaults: core.Resources{MemoryBytes: int64(opts.Int("memory_mb", 4096)) << 20, CPUs: float64(opts.Int("cpus", 2)), PIDs: int64(opts.Int("pids", 512))},
		})
	})
}

// Info implements core.Provider. Privileged mode, GPUs and KVM are deliberately
// not offered, so workloads asking for them are refused.
func (r *Runtime) Info() core.ProviderInfo {
	caps := []core.Capability{core.CapRuntimeVolumes, core.CapRuntimeNetworkNone, core.CapRuntimeNetworkEgress, core.CapRuntimeResourceLimits, core.CapRuntimeReadOnlyRoot}
	if r.opts.DiskLimits {
		caps = append(caps, core.CapRuntimeDiskLimits)
	}
	return core.ProviderInfo{Kind: core.ProviderRuntime, Type: typeName, Name: r.name, ContractVersion: core.ContractVersion, Capabilities: caps}
}

func (r *Runtime) call(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, core.Wrap(core.KindInternal, "docker.call", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.base+path, body)
	if err != nil {
		return 0, core.Wrap(core.KindInternal, "docker.call", err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, core.Wrap(core.KindCancelled, "docker.call", ctx.Err())
		}
		return 0, core.Wrap(core.KindTransient, "docker.call", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, core.Wrap(core.KindTransient, "docker.call", err)
	}
	if resp.StatusCode >= 300 {
		return resp.StatusCode, apiError(method, path, resp.StatusCode, data)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, core.Wrap(core.KindTransient, "docker.call", err)
		}
	}
	return resp.StatusCode, nil
}

func apiError(method, path string, status int, data []byte) error {
	var msg struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(data, &msg)
	route, _, _ := strings.Cut(path, "?")
	text := fmt.Sprintf("Docker returned %d for %s %s: %s", status, method, route, msg.Message)
	switch {
	case status == http.StatusNotModified:
		return nil
	case status == http.StatusNotFound:
		return core.Errorf(core.KindNotFound, "docker.call", "%s", text)
	case status == http.StatusConflict:
		return core.Errorf(core.KindConflict, "docker.call", "%s", text)
	case status == http.StatusBadRequest:
		return core.Errorf(core.KindInvalid, "docker.call", "%s", text)
	case status == http.StatusForbidden, status == http.StatusUnauthorized:
		return core.Errorf(core.KindForbidden, "docker.call", "%s", text)
	case status >= 500:
		return core.Errorf(core.KindTransient, "docker.call", "%s", text)
	}
	return core.Errorf(core.KindInternal, "docker.call", "%s", text)
}

type mountSpec struct {
	Type     string `json:"Type"`
	Source   string `json:"Source"`
	Target   string `json:"Target"`
	ReadOnly bool   `json:"ReadOnly"`
}

type hostConfig struct {
	Mounts         []mountSpec       `json:"Mounts,omitempty"`
	NetworkMode    string            `json:"NetworkMode"`
	Memory         int64             `json:"Memory,omitempty"`
	MemorySwap     int64             `json:"MemorySwap,omitempty"`
	NanoCpus       int64             `json:"NanoCpus,omitempty"`
	PidsLimit      int64             `json:"PidsLimit,omitempty"`
	CapDrop        []string          `json:"CapDrop"`
	SecurityOpt    []string          `json:"SecurityOpt"`
	ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
	Tmpfs          map[string]string `json:"Tmpfs,omitempty"`
	Privileged     bool              `json:"Privileged"`
	Init           bool              `json:"Init"`
	StorageOpt     map[string]string `json:"StorageOpt,omitempty"`
}

type containerCreate struct {
	Image      string            `json:"Image"`
	Cmd        []string          `json:"Cmd"`
	Entrypoint []string          `json:"Entrypoint"`
	Env        []string          `json:"Env"`
	WorkingDir string            `json:"WorkingDir,omitempty"`
	User       string            `json:"User"`
	Labels     map[string]string `json:"Labels"`
	HostConfig hostConfig        `json:"HostConfig"`
}

// buildCreate is pure, which lets tests assert the security posture of the request
// sent to the engine without needing one.
func (r *Runtime) buildCreate(spec core.WorkloadSpec, deadline time.Time) (containerCreate, error) {
	if spec.Image == "" || len(spec.Command) == 0 {
		return containerCreate{}, core.Errorf(core.KindInvalid, "docker.create", "workload needs an image and a command")
	}
	res := spec.Resources
	if res.MemoryBytes == 0 {
		res.MemoryBytes = r.opts.Defaults.MemoryBytes
	}
	if res.CPUs == 0 {
		res.CPUs = r.opts.Defaults.CPUs
	}
	if res.PIDs == 0 {
		res.PIDs = r.opts.Defaults.PIDs
	}
	hc := hostConfig{
		Memory: res.MemoryBytes, MemorySwap: res.MemoryBytes, NanoCpus: int64(res.CPUs * 1e9), PidsLimit: res.PIDs,
		CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"}, Init: true,
		ReadonlyRootfs: spec.Security.ReadOnlyRoot,
		Tmpfs:          map[string]string{"/tmp": tmpfsOptions},
	}
	for _, m := range spec.Mounts {
		if !volumeName.MatchString(m.Volume) {
			return containerCreate{}, core.Errorf(core.KindInvalid, "docker.create", "%q is not a volume name; host paths cannot be mounted", m.Volume)
		}
		hc.Mounts = append(hc.Mounts, mountSpec{Type: "volume", Source: m.Volume, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	switch spec.Network {
	case core.NetworkNone:
		hc.NetworkMode = "none"
	case core.NetworkEgress:
		hc.NetworkMode = r.opts.Network
	}
	if spec.Resources.DiskBytes > 0 {
		hc.StorageOpt = map[string]string{"size": strconv.FormatInt(spec.Resources.DiskBytes, 10)}
	}
	labels := maps.Clone(spec.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	if !deadline.IsZero() {
		labels[labelDeadline] = strconv.FormatInt(deadline.Unix(), 10)
	}
	var env []string
	for _, k := range slices.Sorted(maps.Keys(spec.Env)) {
		env = append(env, k+"="+spec.Env[k])
	}
	for _, k := range slices.Sorted(maps.Keys(spec.Secrets)) {
		env = append(env, k+"="+spec.Secrets[k].Reveal())
	}
	return containerCreate{
		Image: spec.Image, Cmd: spec.Command, Entrypoint: []string{}, Env: env, WorkingDir: spec.WorkDir, User: r.opts.User, Labels: labels, HostConfig: hc,
	}, nil
}

// Create implements core.ExecutionRuntime.
func (r *Runtime) Create(ctx context.Context, spec core.WorkloadSpec) (core.WorkloadStatus, error) {
	if err := core.CheckWorkloadSpec(r, spec); err != nil {
		return core.WorkloadStatus{}, err
	}
	var deadline time.Time
	if spec.Timeout > 0 {
		deadline = time.Now().Add(spec.Timeout)
	}
	body, err := r.buildCreate(spec, deadline)
	if err != nil {
		return core.WorkloadStatus{}, err
	}
	if spec.Network == core.NetworkEgress {
		if err := r.ensureNetwork(ctx); err != nil {
			return core.WorkloadStatus{}, err
		}
	}
	path := "/containers/create?name=" + url.QueryEscape(spec.Name)
	_, err = r.call(ctx, http.MethodPost, path, body, nil)
	if core.IsKind(err, core.KindNotFound) && r.opts.PullMissing {
		if perr := r.pull(ctx, spec.Image); perr != nil {
			return core.WorkloadStatus{}, perr
		}
		_, err = r.call(ctx, http.MethodPost, path, body, nil)
	}
	switch {
	case err == nil:
	case core.IsKind(err, core.KindConflict):
		// Created by an earlier call. Only one that never started may be started: starting
		// a finished container would run it a second time.
		st, serr := r.Status(ctx, spec.Name)
		if serr != nil {
			return core.WorkloadStatus{}, serr
		}
		if st.Phase != core.WorkloadPending {
			return st, nil
		}
	default:
		return core.WorkloadStatus{}, err
	}
	if _, err := r.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(spec.Name)+"/start", nil, nil); err != nil {
		return core.WorkloadStatus{}, err
	}
	if spec.Timeout > 0 {
		time.AfterFunc(spec.Timeout, func() {
			c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = r.stop(c, spec.Name)
		})
	}
	return r.Status(ctx, spec.Name)
}

func (r *Runtime) ensureNetwork(ctx context.Context) error {
	r.mu.Lock()
	ok := r.netOK
	r.mu.Unlock()
	if ok {
		return nil
	}
	_, err := r.call(ctx, http.MethodGet, "/networks/"+url.PathEscape(r.opts.Network), nil, nil)
	if core.IsKind(err, core.KindNotFound) {
		_, err = r.call(ctx, http.MethodPost, "/networks/create", map[string]any{
			"Name": r.opts.Network, "Driver": "bridge", "CheckDuplicate": true,
			"Labels":  map[string]string{"goblins.managed": "true"},
			"Options": map[string]string{"com.docker.network.bridge.enable_icc": "false"},
		}, nil)
		if core.IsKind(err, core.KindConflict) {
			err = nil
		}
	}
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.netOK = true
	r.mu.Unlock()
	return nil
}

// imageRef splits a reference into what the pull endpoint takes, whose tag parameter
// may also hold a digest. A digest pins the content, so any tag beside it is dropped.
func imageRef(image string) (name, tag string) {
	repo, digest, pinned := strings.Cut(image, "@")
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		name, tag = repo[:i], repo[i+1:]
	} else {
		name, tag = repo, "latest"
	}
	if pinned {
		tag = digest
	}
	return name, tag
}

func (r *Runtime) pull(ctx context.Context, image string) error {
	name, tag := imageRef(image)
	q := url.Values{"fromImage": {name}, "tag": {tag}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.base+"/images/create?"+q.Encode(), nil)
	if err != nil {
		return core.Wrap(core.KindInternal, "docker.pull", err)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return core.Wrap(core.KindTransient, "docker.pull", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return core.Errorf(core.KindInvalid, "docker.pull", "image %s is not available on the engine and cannot be pulled", image)
	}
	// The stream reports errors in-band, after a 200.
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var line struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &line) == nil && line.Error != "" {
			return core.Errorf(core.KindInvalid, "docker.pull", "image %s cannot be pulled", image)
		}
	}
	return nil
}

type inspect struct {
	State struct {
		Status     string    `json:"Status"`
		ExitCode   int       `json:"ExitCode"`
		FinishedAt time.Time `json:"FinishedAt"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

// Status implements core.ExecutionRuntime. It also enforces deadlines recorded on
// the container, so a limit outlives a restart of this process.
func (r *Runtime) Status(ctx context.Context, name string) (core.WorkloadStatus, error) {
	var in inspect
	if _, err := r.call(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil, &in); err != nil {
		return core.WorkloadStatus{}, err
	}
	st := core.WorkloadStatus{Name: name, Labels: in.Config.Labels, ExitCode: in.State.ExitCode}
	r.mu.Lock()
	cancelled := r.cancel[name]
	r.mu.Unlock()
	var deadline time.Time
	if v, err := strconv.ParseInt(in.Config.Labels[labelDeadline], 10, 64); err == nil {
		deadline = time.Unix(v, 0)
	}
	switch in.State.Status {
	case "created":
		st.Phase = core.WorkloadPending
	case "running", "paused", "restarting", "removing":
		st.Phase = core.WorkloadRunning
		if !deadline.IsZero() && time.Now().After(deadline) {
			_ = r.stop(ctx, name)
		}
	default:
		switch {
		case cancelled:
			st.Phase = core.WorkloadCancelled
		case in.State.ExitCode == 0:
			st.Phase = core.WorkloadSucceeded
		case !deadline.IsZero() && !in.State.FinishedAt.Before(deadline):
			st.Phase = core.WorkloadTimedOut
		default:
			st.Phase = core.WorkloadFailed
		}
	}
	return st, nil
}

// demux splits Docker's multiplexed log stream onto two writers.
func demux(src io.Reader, stdout, stderr io.Writer) error {
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(src, hdr); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		size := int64(binary.BigEndian.Uint32(hdr[4:8]))
		dst := stdout
		if hdr[0] == 2 {
			dst = stderr
		}
		if _, err := io.CopyN(dst, src, size); err != nil {
			return err
		}
	}
}

// Logs implements core.ExecutionRuntime.
func (r *Runtime) Logs(ctx context.Context, name string, follow bool, stdout, stderr io.Writer) error {
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "follow": {strconv.FormatBool(follow)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+"/containers/"+url.PathEscape(name)+"/logs?"+q.Encode(), nil)
	if err != nil {
		return core.Wrap(core.KindInternal, "docker.logs", err)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return core.Wrap(core.KindCancelled, "docker.logs", ctx.Err())
		}
		return core.Wrap(core.KindTransient, "docker.logs", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return apiError(http.MethodGet, "/containers/logs", resp.StatusCode, data)
	}
	if err := demux(resp.Body, stdout, stderr); err != nil {
		if ctx.Err() != nil {
			return core.Wrap(core.KindCancelled, "docker.logs", ctx.Err())
		}
		return core.Wrap(core.KindTransient, "docker.logs", err)
	}
	return nil
}

func (r *Runtime) stop(ctx context.Context, name string) error {
	_, err := r.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/stop?t=5", nil, nil)
	return err
}

// Cancel implements core.ExecutionRuntime.
func (r *Runtime) Cancel(ctx context.Context, name string) error {
	st, err := r.Status(ctx, name)
	if err != nil {
		return err
	}
	if st.Phase.Terminal() {
		return nil
	}
	r.mu.Lock()
	r.cancel[name] = true
	r.mu.Unlock()
	return r.stop(ctx, name)
}

// Cleanup implements core.ExecutionRuntime.
func (r *Runtime) Cleanup(ctx context.Context, name string) error {
	_, err := r.call(ctx, http.MethodDelete, "/containers/"+url.PathEscape(name)+"?force=1&v=1", nil, nil)
	if core.IsKind(err, core.KindNotFound) {
		err = nil
	}
	if err == nil {
		r.mu.Lock()
		delete(r.cancel, name)
		r.mu.Unlock()
	}
	return err
}

// List implements core.ExecutionRuntime.
func (r *Runtime) List(ctx context.Context, labels map[string]string) ([]core.WorkloadStatus, error) {
	var f []string
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		f = append(f, k+"="+labels[k])
	}
	filters, err := json.Marshal(map[string][]string{"label": f})
	if err != nil {
		return nil, core.Wrap(core.KindInternal, "docker.list", err)
	}
	var out []struct {
		Names  []string          `json:"Names"`
		Labels map[string]string `json:"Labels"`
	}
	if _, err := r.call(ctx, http.MethodGet, "/containers/json?all=1&filters="+url.QueryEscape(string(filters)), nil, &out); err != nil {
		return nil, err
	}
	res := make([]core.WorkloadStatus, 0, len(out))
	for _, c := range out {
		if len(c.Names) == 0 {
			continue
		}
		st, err := r.Status(ctx, strings.TrimPrefix(c.Names[0], "/"))
		if core.IsKind(err, core.KindNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		res = append(res, st)
	}
	return res, nil
}

// EnsureVolume implements core.ExecutionRuntime. Creating an existing volume is a
// no-op on the engine.
func (r *Runtime) EnsureVolume(ctx context.Context, name string, labels map[string]string) error {
	if !volumeName.MatchString(name) {
		return core.Errorf(core.KindInvalid, "docker.volume", "invalid volume name %q", name)
	}
	_, err := r.call(ctx, http.MethodPost, "/volumes/create", map[string]any{"Name": name, "Labels": labels}, nil)
	return err
}

// RemoveVolume implements core.ExecutionRuntime.
func (r *Runtime) RemoveVolume(ctx context.Context, name string) error {
	_, err := r.call(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), nil, nil)
	if core.IsKind(err, core.KindNotFound) {
		return nil
	}
	return err
}
