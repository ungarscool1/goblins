package docker

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/ungarscool1/goblins/internal/core"
)

const (
	labelWorkload = "goblins.workload"
	labelService  = "goblins.service"
)

// servicesNetwork is the private network a workload shares with its services. It is internal:
// nothing on it reaches anything else.
func servicesNetwork(workload string) string { return workload + "-services" }

func serviceContainer(workload, service string) string { return workload + "-svc-" + service }

type serviceCreate struct {
	Image      string            `json:"Image"`
	Cmd        []string          `json:"Cmd,omitempty"`
	Env        []string          `json:"Env"`
	User       string            `json:"User"`
	Labels     map[string]string `json:"Labels"`
	HostConfig hostConfig        `json:"HostConfig"`
	Networking struct {
		Endpoints map[string]struct {
			Aliases []string `json:"Aliases"`
		} `json:"EndpointsConfig"`
	} `json:"NetworkingConfig"`
}

// buildService is pure, like buildCreate. A service keeps its image's entrypoint and a writable
// root, as databases need one, and nothing else of the workload but its labels: no mounts, no
// secrets. The labels say who owns it, so reconciliation finds it even if the workload's own
// container was never created.
func (r *Runtime) buildService(spec core.WorkloadSpec, s core.Service) serviceCreate {
	workload := spec.Name
	res := s.Resources
	if res.MemoryBytes == 0 {
		res.MemoryBytes = 1 << 30
	}
	if res.CPUs == 0 {
		res.CPUs = 1
	}
	if res.PIDs == 0 {
		res.PIDs = r.opts.Defaults.PIDs
	}
	labels := maps.Clone(spec.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	maps.Copy(labels, map[string]string{"goblins.managed": "true", labelWorkload: workload,
		labelService: s.Name})
	c := serviceCreate{Image: s.Image, Cmd: s.Command, User: r.opts.User, Labels: labels,
		HostConfig: hostConfig{NetworkMode: servicesNetwork(workload), Memory: res.MemoryBytes,
			MemorySwap: res.MemoryBytes, NanoCpus: int64(res.CPUs * 1e9), PidsLimit: res.PIDs,
			CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"}, Init: true,
			Tmpfs: map[string]string{"/tmp": tmpfsOptions}}}
	for _, k := range slices.Sorted(maps.Keys(s.Env)) {
		c.Env = append(c.Env, k+"="+s.Env[k])
	}
	c.Networking.Endpoints = map[string]struct {
		Aliases []string `json:"Aliases"`
	}{servicesNetwork(workload): {Aliases: []string{s.Name}}}
	return c
}

// startServices creates the workload's network and starts its services. It is idempotent, as
// Create is.
func (r *Runtime) startServices(ctx context.Context, spec core.WorkloadSpec) error {
	if len(spec.Services) == 0 {
		return nil
	}
	_, err := r.call(ctx, http.MethodPost, "/networks/create", map[string]any{
		"Name": servicesNetwork(spec.Name), "Driver": "bridge", "Internal": true,
		"CheckDuplicate": true,
		"Labels":         map[string]string{"goblins.managed": "true", labelWorkload: spec.Name}},
		nil)
	if err != nil && !core.IsKind(err, core.KindConflict) {
		return err
	}
	for _, s := range spec.Services {
		name := serviceContainer(spec.Name, s.Name)
		path := "/containers/create?name=" + url.QueryEscape(name)
		body := r.buildService(spec, s)
		_, err := r.call(ctx, http.MethodPost, path, body, nil)
		if core.IsKind(err, core.KindNotFound) && r.opts.PullMissing {
			if err = r.pull(ctx, s.Image); err == nil {
				_, err = r.call(ctx, http.MethodPost, path, body, nil)
			}
		}
		if err != nil && !core.IsKind(err, core.KindConflict) {
			return err
		}
		code, err := r.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/start",
			nil, nil)
		if err != nil && code != http.StatusNotModified { // already running, on a retry
			return err
		}
	}
	return nil
}

// removeServices removes a workload's services, found by label, and their network, so nothing
// is left even when the workload's spec is gone. For a workload without services, both are
// one request that finds nothing.
func (r *Runtime) removeServices(ctx context.Context, workload string) error {
	filters, _ := json.Marshal(map[string][]string{"label": {labelWorkload + "=" + workload}})
	var out []struct {
		Names []string `json:"Names"`
	}
	q := "?all=1&filters=" + url.QueryEscape(string(filters))
	if _, err := r.call(ctx, http.MethodGet, "/containers/json"+q, nil, &out); err != nil {
		return err
	}
	for _, c := range out {
		if len(c.Names) == 0 {
			continue
		}
		name := strings.TrimPrefix(c.Names[0], "/")
		_, err := r.call(ctx, http.MethodDelete, "/containers/"+url.PathEscape(name)+"?force=1&v=1",
			nil, nil)
		if err != nil && !core.IsKind(err, core.KindNotFound) {
			return err
		}
	}
	_, err := r.call(ctx, http.MethodDelete, "/networks/"+url.PathEscape(servicesNetwork(workload)),
		nil, nil)
	if core.IsKind(err, core.KindNotFound) {
		return nil
	}
	return err
}

// orphanServices stands for a workload of which only services remain. It has failed: what
// it was to run is not there.
func orphanServices(workload string, labels map[string]string) core.WorkloadStatus {
	l := maps.Clone(labels)
	delete(l, labelService)
	return core.WorkloadStatus{Name: workload, Phase: core.WorkloadFailed, ExitCode: -1, Labels: l}
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
	seen := map[string]bool{}
	for _, c := range out {
		if len(c.Names) == 0 || c.Labels[labelService] != "" {
			continue
		}
		seen[strings.TrimPrefix(c.Names[0], "/")] = true
	}
	for _, c := range out {
		if len(c.Names) == 0 {
			continue
		}
		if c.Labels[labelService] != "" {
			// A service whose workload's container is gone, or never came: the workload is
			// reported in its place, so that cleaning it up removes the service and network.
			if w := c.Labels[labelWorkload]; w != "" && !seen[w] {
				seen[w] = true
				res = append(res, orphanServices(w, c.Labels))
			}
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
