package docker

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"slices"

	"github.com/ungarscool1/goblins/internal/core"
)

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

// ListVolumes implements core.VolumeLister.
func (r *Runtime) ListVolumes(ctx context.Context,
	labels map[string]string) ([]core.VolumeStatus, error) {
	var f []string
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		f = append(f, k+"="+labels[k])
	}
	filters, err := json.Marshal(map[string][]string{"label": f})
	if err != nil {
		return nil, core.Wrap(core.KindInternal, "docker.volumes", err)
	}
	var out struct {
		Volumes []struct {
			Name   string            `json:"Name"`
			Labels map[string]string `json:"Labels"`
		} `json:"Volumes"`
	}
	path := "/volumes?filters=" + url.QueryEscape(string(filters))
	if _, err := r.call(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	res := make([]core.VolumeStatus, 0, len(out.Volumes))
	for _, v := range out.Volumes {
		res = append(res, core.VolumeStatus{Name: v.Name, Labels: v.Labels})
	}
	return res, nil
}
