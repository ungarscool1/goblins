package core

import (
	"fmt"
	"slices"
	"strings"
)

// ContractVersion is bumped on breaking changes to the provider contracts.
// Adapters report the version they were written against, and the registry
// refuses adapters built for another version.
const ContractVersion = 1

// ProviderKind identifies a family of interchangeable adapters.
type ProviderKind string

const (
	ProviderTracker      ProviderKind = "tracker"
	ProviderSCM          ProviderKind = "scm"
	ProviderEventSource  ProviderKind = "event_source"
	ProviderAgent        ProviderKind = "agent"
	ProviderRuntime      ProviderKind = "runtime"
	ProviderReview       ProviderKind = "review"
	ProviderValidation   ProviderKind = "validation"
	ProviderNotification ProviderKind = "notification"
)

// Capability names an optional feature of an adapter. Orchestration code asks
// for capabilities, never for a specific provider.
type Capability string

const (
	CapTrackerCommentWrite Capability = "tracker.comment.write"
	CapTrackerLabels       Capability = "tracker.labels"
	CapTrackerTransitions  Capability = "tracker.transitions"
	CapTrackerPermissions  Capability = "tracker.permissions"

	CapSCMPullRequests Capability = "scm.pull_requests"
	CapSCMDraft        Capability = "scm.pull_requests.draft"
	CapSCMInlineReview Capability = "scm.reviews.inline"
	CapSCMStatusChecks Capability = "scm.status_checks"

	CapEventsWebhook Capability = "events.webhook"
	CapEventsPoll    Capability = "events.poll"

	CapAgentResume    Capability = "agent.resume"
	CapAgentStreaming Capability = "agent.streaming"
	CapAgentEffort    Capability = "agent.effort"

	// CapReviewWorkspace asks the orchestrator for a pinned checkout and diff.
	CapReviewWorkspace Capability = "review.workspace"

	CapRuntimeVolumes        Capability = "runtime.volumes"
	CapRuntimeNetworkNone    Capability = "runtime.network.none"
	CapRuntimeNetworkEgress  Capability = "runtime.network.egress"
	CapRuntimeResourceLimits Capability = "runtime.resources.limits"
	CapRuntimeDiskLimits     Capability = "runtime.resources.disk"
	CapRuntimeReadOnlyRoot   Capability = "runtime.security.read_only_root"
	CapRuntimePrivileged     Capability = "runtime.security.privileged"
	CapRuntimeGPU            Capability = "runtime.devices.gpu"
	CapRuntimeKVM            Capability = "runtime.devices.kvm"
)

// ProviderInfo describes one adapter instance.
type ProviderInfo struct {
	Kind            ProviderKind
	Type            string // adapter implementation, e.g. the value of `type:` in configuration
	Name            string // configured instance name
	ContractVersion int
	Capabilities    []Capability
}

// Provider is implemented by every adapter.
type Provider interface {
	Info() ProviderInfo
}

// Has reports whether the provider advertises capability c.
func (i ProviderInfo) Has(c Capability) bool { return slices.Contains(i.Capabilities, c) }

// Negotiate returns an unsupported error naming every required capability the
// provider lacks. The message is user-facing: it ends up on the task's status.
func Negotiate(p Provider, required ...Capability) error {
	info := p.Info()
	var missing []string
	for _, c := range required {
		if !info.Has(c) {
			missing = append(missing, string(c))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return Errorf(KindUnsupported, "capability.negotiate", "%s provider %q (%s) does not support: %s",
		info.Kind, info.Name, info.Type, strings.Join(missing, ", "))
}

// Unsupported builds the error adapters must return from an operation whose
// capability they do not advertise, instead of silently succeeding.
func Unsupported(info ProviderInfo, c Capability) error {
	return Errorf(KindUnsupported, string(c), "%s provider %q (%s) does not support %s", info.Kind, info.Name, info.Type, c)
}

// Secret holds a credential in memory. Its formatted, logged and serialized
// forms are redacted; only Reveal returns the value.
type Secret struct{ value string }

// NewSecret wraps a credential.
func NewSecret(v string) Secret { return Secret{value: v} }

// Reveal returns the credential. Call it only where it is handed to its target.
func (s Secret) Reveal() string { return s.value }

// Empty reports whether no credential is set.
func (s Secret) Empty() bool { return s.value == "" }

func (s Secret) String() string { return "[REDACTED]" }

// GoString implements fmt.GoStringer.
func (s Secret) GoString() string { return "core.Secret{[REDACTED]}" }

// Format implements fmt.Formatter so that no verb, including %#v and %+v on an
// enclosing struct, can print the value.
func (s Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte("[REDACTED]")) }

// MarshalJSON implements json.Marshaler.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }
