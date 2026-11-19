package kubernetes

import "fmt"

const (
	defaultGuardImage = "alpine:3.22"
	guardAttempts     = 120
)

// networkGuard holds a pod with no network until its network policy is in force. Some policy
// controllers, such as kube-router in k3s, program a new pod's rules a second or two after it
// starts, which would let a check's code reach out in that window. The guard runs first, in the
// pod's network, and waits until the API server, which every pod can otherwise reach, is not
// reachable; if that never happens, the pod fails rather than run without isolation.
func (r *Runtime) networkGuard(res *resources) containerSpec {
	f := false
	script := fmt.Sprintf(`i=0
while nc -z -w 1 "$KUBERNETES_SERVICE_HOST" "$KUBERNETES_SERVICE_PORT" 2>/dev/null; do
  i=$((i+1))
  if [ "$i" -ge %d ]; then echo "network isolation is not in force"; exit 1; fi
  sleep 0.5
done`, guardAttempts)
	return containerSpec{Name: "network-guard", Image: r.o.GuardImage,
		Command: []string{"sh", "-c", script}, ImagePullPolicy: r.o.PullPolicy,
		Resources: res,
		SecurityContext: containerSecurity{AllowPrivilegeEscalation: &f, Privileged: &f,
			ReadOnlyRoot: true, Capabilities: dropAll}}
}
