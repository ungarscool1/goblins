package docker

import (
	"context"
	"net/http"
	"net/url"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/runtime"
)

const defaultEgressNetwork = "goblins-egress"

func (r *Runtime) egressNetwork() string {
	if r.opts.EgressProxy == "" {
		return r.opts.Network
	}
	if r.opts.EgressNetwork != "" {
		return r.opts.EgressNetwork
	}
	return defaultEgressNetwork
}

func (r *Runtime) proxyEnv(n core.NetworkPolicy) []string {
	if n != core.NetworkEgress {
		return nil
	}
	return runtime.ProxyEnv(r.opts.EgressProxy)
}

// ensureEgress makes sure the network egress workloads join exists. Behind a proxy it has to
// be internal, with no route out, or the proxy would only be a suggestion; one that exists and
// is not internal is refused rather than used.
func (r *Runtime) ensureEgress(ctx context.Context) error {
	if r.opts.EgressProxy == "" {
		return r.ensureNetwork(ctx)
	}
	r.mu.Lock()
	ok := r.netOK
	r.mu.Unlock()
	if ok {
		return nil
	}
	name := r.egressNetwork()
	var nw struct {
		Internal bool `json:"Internal"`
	}
	_, err := r.call(ctx, http.MethodGet, "/networks/"+url.PathEscape(name), nil, &nw)
	switch {
	case core.IsKind(err, core.KindNotFound):
		_, err = r.call(ctx, http.MethodPost, "/networks/create", map[string]any{
			"Name": name, "Driver": "bridge", "Internal": true, "CheckDuplicate": true,
			"Labels": map[string]string{"goblins.managed": "true"}}, nil)
		if core.IsKind(err, core.KindConflict) {
			err = nil
		}
	case err == nil && !nw.Internal:
		return core.Errorf(core.KindInvalid, "docker.network",
			"network %s must be internal when egress_proxy is set, or workloads could bypass it",
			name)
	}
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.netOK = true
	r.mu.Unlock()
	return nil
}
