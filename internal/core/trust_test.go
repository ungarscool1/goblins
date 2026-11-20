package core

import "testing"

func TestSandboxesHoldNothing(t *testing.T) {
	login := Mount{Volume: "agent-login", Target: "/c", Credentials: true}
	for name, c := range map[string]struct {
		spec WorkloadSpec
		kind ErrorKind
	}{
		"sandbox with a secret": {WorkloadSpec{Secrets: map[string]Secret{"T": NewSecret("x")}},
			KindForbidden},
		"sandbox with the login": {WorkloadSpec{Mounts: []Mount{login}}, KindForbidden},
		"unknown trust":          {WorkloadSpec{Trust: "root"}, KindInvalid},
		"credentialed agent": {WorkloadSpec{Trust: TrustCredentialed, Mounts: []Mount{login},
			Secrets: map[string]Secret{"T": NewSecret("x")}}, ""},
		"plain sandbox": {WorkloadSpec{Mounts: []Mount{{Volume: "scratch", Target: "/s"}}}, ""},
	} {
		err := checkTrust(c.spec)
		if c.kind == "" && err != nil || c.kind != "" && !IsKind(err, c.kind) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
