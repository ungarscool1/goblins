package core

// WorkloadTrust separates the two kinds of workload ADR 0004 keeps apart. The zero value is
// the most confined, so a workload nobody classified can hold nothing.
type WorkloadTrust string

const (
	// TrustSandbox may run code from a repository, so it holds no secret and mounts no login.
	TrustSandbox WorkloadTrust = ""
	// TrustCredentialed holds credentials, an agent's login or a short-lived SCM token, and
	// never runs repository code: an agent restricted to file tools, or a Git step with hooks
	// and repository configuration ignored.
	TrustCredentialed WorkloadTrust = "credentialed"
)

// checkTrust enforces what a workload's trust allows, for every runtime alike, before anything
// starts. It is the contract's own copy of the rule, so a mistake in one caller cannot give a
// sandbox a credential.
func checkTrust(spec WorkloadSpec) error {
	switch spec.Trust {
	case TrustCredentialed:
		return nil
	case TrustSandbox:
	default:
		return Errorf(KindInvalid, "runtime.create", "workload %s has an unknown trust %q",
			spec.Name, spec.Trust)
	}
	if len(spec.Secrets) > 0 {
		return Errorf(KindForbidden, "runtime.create",
			"workload %s is a sandbox and cannot be given secrets", spec.Name)
	}
	for _, m := range spec.Mounts {
		if m.Credentials {
			return Errorf(KindForbidden, "runtime.create",
				"workload %s is a sandbox and cannot mount the login in %s", spec.Name, m.Volume)
		}
	}
	return nil
}

// CredentialHolder is implemented by agents that keep a login in volumes, so that runtimes
// can refuse those volumes to sandboxes even when a mount forgets to say what it holds.
type CredentialHolder interface {
	CredentialVolumes() []string
}
