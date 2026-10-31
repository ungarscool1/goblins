package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

type stub struct{ caps []Capability }

func (s stub) Info() ProviderInfo {
	return ProviderInfo{Kind: ProviderTracker, Type: "stub", Name: "s", Capabilities: s.caps}
}

func TestNegotiate(t *testing.T) {
	p := stub{caps: []Capability{CapTrackerLabels}}
	if err := Negotiate(p, CapTrackerLabels); err != nil {
		t.Fatal(err)
	}
	err := Negotiate(p, CapTrackerLabels, CapTrackerCommentWrite, CapTrackerPermissions)
	if !IsKind(err, KindUnsupported) {
		t.Fatalf("kind = %s", KindOf(err))
	}
	msg := UserMessage(err)
	if !strings.Contains(msg, string(CapTrackerCommentWrite)) || !strings.Contains(msg, string(CapTrackerPermissions)) || strings.Contains(msg, string(CapTrackerLabels)) {
		t.Fatalf("message = %q", msg)
	}
}

func TestSecretCannotLeak(t *testing.T) {
	s := NewSecret("hunter2")
	type wrap struct{ Password Secret }
	for _, out := range []string{
		fmt.Sprint(s), fmt.Sprintf("%v %+v %#v %s %q %x", s, s, s, s, s, s),
		fmt.Sprintf("%+v %#v", wrap{s}, wrap{s}),
	} {
		if strings.Contains(out, "hunter2") {
			t.Fatalf("leaked: %s", out)
		}
	}
	b, _ := json.Marshal(wrap{s})
	if strings.Contains(string(b), "hunter2") {
		t.Fatalf("json leaked: %s", b)
	}
	if s.Reveal() != "hunter2" {
		t.Fatal("Reveal broken")
	}
}

func TestRequirementsHashIgnoresNonMaterialFields(t *testing.T) {
	a := Issue{Title: "t", Body: "b", Labels: []string{"x"}}
	b := Issue{Title: "t", Body: "b"}
	if a.RequirementsHash() != b.RequirementsHash() {
		t.Fatal("labels changed the hash")
	}
	if a.RequirementsHash() == (Issue{Title: "tb", Body: ""}).RequirementsHash() {
		t.Fatal("field boundaries ignored")
	}
}

func TestSubjectKeyAndPermissions(t *testing.T) {
	s := Subject{Kind: SubjectIssue, Provider: "p", Container: "c", ExternalID: "1"}
	if s.Key() != "issue/p/c/1" {
		t.Fatal(s.Key())
	}
	if ParsePermission("write") != PermissionWrite || ParsePermission("bogus") != PermissionNone {
		t.Fatal("ParsePermission wrong")
	}
	if PermissionAdmin <= PermissionWrite || PermissionWrite <= PermissionTriage {
		t.Fatal("permissions unordered")
	}
}
