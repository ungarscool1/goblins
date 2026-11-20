package config

// Approval profiles. Strict, the default, has every design wait for a person.
const (
	ApprovalStrict     = "strict"
	ApprovalUnattended = "unattended"
)

// SensitivePaths are always out of an unattended scope: automation, Goblins' own files,
// infrastructure, credentials and authentication, and dependencies.
var SensitivePaths = []string{".github/**", ".gitlab-ci.yml", ".goblins/**", "**/Dockerfile",
	"**/*.tf", "**/*.tfvars", "deploy/**", "infra/**", "terraform/**", "charts/**", "helm/**",
	"**/auth/**", "**/security/**", "**/*secret*", "**/*credential*", "**/.env*", "**/*.pem",
	"**/*.key", "go.mod", "go.sum", "package.json", "**/package-lock.json", "**/pnpm-lock.yaml"}

// Approval says when a design may be implemented without a person approving it. Unattended
// is for a constrained scope only: a person with approval permission still invokes it per item
// with Label, and a change touching anything outside AllowedPaths, or inside DeniedPaths and
// the built-in sensitive paths, stops for a person before anything is published.
type Approval struct {
	Profile         string   `yaml:"profile,omitempty"`
	Label           string   `yaml:"label,omitempty"`
	AllowedPaths    []string `yaml:"allowed_paths,omitempty"`
	DeniedPaths     []string `yaml:"denied_paths,omitempty"`
	MaxChangedFiles int      `yaml:"max_changed_files,omitempty"`
}

func (a Approval) validate(path, intakeLabel string, add func(string, ...any)) {
	switch a.Profile {
	case "", ApprovalStrict:
		if a.Label != "" || len(a.AllowedPaths) > 0 {
			add("%s.policy.approval: label and allowed_paths only apply to the unattended profile",
				path)
		}
		return
	case ApprovalUnattended:
	default:
		add("%s.policy.approval.profile: must be strict or unattended", path)
		return
	}
	if a.Label == "" || len(a.AllowedPaths) == 0 {
		add("%s.policy.approval: the unattended profile needs a label and allowed_paths", path)
	}
	if a.Label != "" && a.Label == intakeLabel {
		add("%s.policy.approval.label: must differ from the intake label", path)
	}
	if a.MaxChangedFiles < 0 {
		add("%s.policy.approval.max_changed_files: cannot be negative", path)
	}
}
