package kubernetes

import (
	"os"
	"strings"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
)

// Register adds the runtime to a registry.
func Register(reg *providers.Registry) {
	reg.Register(providers.KindRuntime, typeName, func(name string, deps providers.Deps,
		opts providers.Options) (any, error) {
		// A token file is read again for every request, as Kubernetes rotates projected
		// service account tokens in place. A token from the environment is fixed.
		var tok core.Secret
		if opts.String("token_env") != "" {
			var err error
			if tok, err = opts.Secret(deps, "token"); err != nil {
				return nil, err
			}
		}
		ns := opts.String("namespace")
		if ns == "" && opts.String("api_url") == "" {
			// In a cluster, the namespace Goblins runs in.
			if b, err := os.ReadFile(serviceAccountDir + "/namespace"); err == nil {
				ns = strings.TrimSpace(string(b))
			}
		}
		return New(name, Options{
			Client: ClientOptions{URL: opts.String("api_url"), Token: tok,
				TokenFile: opts.String("token_file"), CAFile: opts.String("ca_file"),
				Getenv: deps.Getenv},
			Namespace: ns, ServiceAccount: opts.String("service_account"),
			UID:             int64(opts.Int("uid", defaultUID)),
			NetworkPolicies: opts.Bool("network_policies", false),
			EgressExcept:    opts.Strings("egress_except"),
			StorageClass:    opts.String("storage_class"),
			VolumeSize:      opts.String("volume_size"), TmpSize: opts.String("tmp_size"),
			PullPolicy: opts.String("pull_policy"), DiskLimits: opts.Bool("disk_limits", true),
			GPUResource: opts.String("gpu_resource"), KVMResource: opts.String("kvm_resource"),
			SplitStreams: opts.Bool("split_streams", false),
			EgressProxy:  opts.String("egress_proxy"), GuardImage: opts.String("guard_image"),
			Defaults: core.Resources{MemoryBytes: int64(opts.Int("memory_mb", 4096)) << 20,
				CPUs: float64(opts.Int("cpus", 2))},
		})
	})
}
