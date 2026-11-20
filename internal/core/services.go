package core

import "regexp"

// Service is a container a workload's tests need, such as a database or a mocked backend. It
// is reachable from the workload by its name, on a network only the two share, and it ends
// with the workload. It runs unprivileged, like the workload, and holds no secret.
type Service struct {
	Name  string
	Image string
	// Command, when set, is given to the image's entrypoint as its arguments, which keeps
	// whatever the entrypoint prepares.
	Command   []string
	Env       map[string]string
	Resources Resources
}

// MaxServices bounds the services of one workload.
const MaxServices = 8

var serviceName = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,30}[a-z0-9])?$`)

// checkServices refuses services that could not be what they say: names that are not host
// names, duplicates, too many, and services on a workload that reaches the internet, whose
// private network they would leave.
func checkServices(spec WorkloadSpec) error {
	if len(spec.Services) == 0 {
		return nil
	}
	if len(spec.Services) > MaxServices {
		return Errorf(KindInvalid, "runtime.create", "workload %s has more than %d services",
			spec.Name, MaxServices)
	}
	if spec.Network != NetworkNone {
		return Errorf(KindInvalid, "runtime.create",
			"workload %s: services need a workload with no network", spec.Name)
	}
	seen := map[string]bool{}
	for _, s := range spec.Services {
		if !serviceName.MatchString(s.Name) || seen[s.Name] || s.Image == "" {
			return Errorf(KindInvalid, "runtime.create",
				"workload %s: service %q needs an image and a unique host name", spec.Name, s.Name)
		}
		seen[s.Name] = true
	}
	return nil
}
