package runtime

import (
	"context"
	"slices"
	"sync"

	"github.com/ungarscool1/goblins/internal/core"
)

// CredentialSet lists the volumes known to hold logins.
type CredentialSet struct {
	mu sync.RWMutex
	v  []string
}

// Add records volumes holding logins.
func (c *CredentialSet) Add(volumes ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, v := range volumes {
		if v != "" && !slices.Contains(c.v, v) {
			c.v = append(c.v, v)
		}
	}
}

// Has reports whether a volume holds a login.
func (c *CredentialSet) Has(volume string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return slices.Contains(c.v, volume)
}

// Guarded refuses to mount a known login volume into a sandbox, whatever the mount says.
// Agents register their volumes before anything runs, so this holds from the first workload.
type Guarded struct {
	core.ExecutionRuntime
	creds *CredentialSet
}

// Guard wraps a runtime.
func Guard(rt core.ExecutionRuntime, creds *CredentialSet) *Guarded {
	return &Guarded{ExecutionRuntime: rt, creds: creds}
}

// Create implements core.ExecutionRuntime.
func (g *Guarded) Create(ctx context.Context, spec core.WorkloadSpec) (core.WorkloadStatus, error) {
	if spec.Trust == core.TrustSandbox {
		for _, m := range spec.Mounts {
			if g.creds.Has(m.Volume) {
				return core.WorkloadStatus{}, core.Errorf(core.KindForbidden, "runtime.create",
					"workload %s is a sandbox and cannot mount %s, which holds a login",
					spec.Name, m.Volume)
			}
		}
	}
	return g.ExecutionRuntime.Create(ctx, spec)
}

// ListVolumes implements core.VolumeLister when the runtime does.
func (g *Guarded) ListVolumes(ctx context.Context,
	labels map[string]string) ([]core.VolumeStatus, error) {
	l, ok := g.ExecutionRuntime.(core.VolumeLister)
	if !ok {
		return nil, core.Unsupported(g.Info(), core.CapRuntimeVolumeList)
	}
	return l.ListVolumes(ctx, labels)
}

// Unwrap returns the runtime underneath.
func (g *Guarded) Unwrap() core.ExecutionRuntime { return g.ExecutionRuntime }
