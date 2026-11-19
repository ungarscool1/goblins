package config

// MergePolicy says what a pull request needs. Goblins never merges unless an auto_merge rule
// expressly allows it, and such a rule cannot do without a person's approval.
type MergePolicy struct {
	RequiredChecks          []string `yaml:"required_checks,omitempty"`
	MinHumanApprovals       int      `yaml:"min_human_approvals,omitempty"`
	Approvers               []string `yaml:"approvers,omitempty"`
	RequireBranchProtection bool     `yaml:"require_branch_protection,omitempty"`
	// SensitivePaths adds to the built-in categories, as category name to path patterns.
	SensitivePaths map[string][]string `yaml:"sensitive_paths,omitempty"`
	AutoMerge      []AutoMergeRule     `yaml:"auto_merge,omitempty"`
}

// AutoMergeRule expressly allows merging a low-risk change touching only these paths.
type AutoMergeRule struct {
	Name     string   `yaml:"name"`
	Paths    []string `yaml:"paths"`
	MaxFiles int      `yaml:"max_files,omitempty"`
}
