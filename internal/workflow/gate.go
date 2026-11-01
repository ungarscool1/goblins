package workflow

import (
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/ungarscool1/goblins/internal/core"
)

// MergePolicy says what a pull request needs before it may merge. The gate reports on it;
// only a person merges, unless an expressly approved rule says otherwise.
type MergePolicy struct {
	RequiredChecks []string
	// MinHumanApprovals counts approvals of the head commit by people. Automation never counts.
	MinHumanApprovals int
	// Approvers, when set, are the only people whose approval counts.
	Approvers               []string
	RequireBranchProtection bool
	// SensitivePaths maps a category to path patterns, where ** spans directories. A change
	// touching one always needs a person, and is never merged automatically.
	SensitivePaths map[string][]string
	// AutoMerge rules are off unless a project lists one.
	AutoMerge []AutoMergeRule
}

// AutoMergeRule expressly allows merging a low-risk change touching only these paths.
type AutoMergeRule struct {
	Name     string
	Paths    []string
	MaxFiles int
}

// DefaultMergePolicy needs one person's approval, and treats common sensitive areas as such.
func DefaultMergePolicy() MergePolicy {
	return MergePolicy{
		MinHumanApprovals: 1,
		SensitivePaths: map[string][]string{
			"authentication": {"**/auth/**", "**/*auth*", "**/*login*", "**/*session*",
				"**/*oauth*", "**/*permission*"},
			"cryptography": {"**/crypto/**", "**/*crypto*", "**/*cipher*", "**/*signature*",
				"**/*.pem", "**/*secret*", "**/*credential*"},
			"network": {"**/net/**", "**/network/**", "**/*firewall*", "**/*proxy*",
				"**/*tls*"},
			"automation": {".*/**", "Dockerfile", "**/Dockerfile", "**/*.tf", "deploy/**",
				"Makefile"},
		},
	}
}

// GateInput is what the gate decides on.
type GateInput struct {
	Task    core.Task
	Result  ReconcileResult
	Policy  MergePolicy
	MinPerm core.Permission
}

// EvaluateGate applies a merge policy to one commit. It is pure, and it counts nothing an
// automated reviewer said: no review is ever a proof of safety.
func EvaluateGate(in GateInput) core.MergeGate {
	pr, sha := in.Result.PullRequest, in.Task.HeadSHA
	gate := core.MergeGate{SHA: sha, Categories: sensitiveCategories(in.Result.Files, in.Policy)}
	var why []string
	if pr.Draft {
		why = append(why, "it is still a draft")
	}
	if a := in.Task.Approval; in.Task.Kind == core.TaskIssue && (a == nil || a.Revision != in.Task.DesignRevision) {
		why = append(why, "the current design revision is not approved")
	}
	if v := in.Task.ValidatedSHA; v != "" && v != sha {
		why = append(why, "this commit has not passed the project's validation")
	}
	why = append(why, checkReasons(in)...)
	why = append(why, reviewReasons(in, sha)...)
	why = append(why, protectionReasons(in)...)
	for _, c := range gate.Categories {
		why = append(why, fmt.Sprintf("it touches %s code, which a person has to look at", c))
	}
	gate.Reasons = why
	gate.State = core.GateWaiting
	// Sensitive areas never go without a person explicitly merging, whatever the approvals.
	if len(why) == len(gate.Categories) {
		gate.State = core.GateReady
	}
	if len(why) == 0 {
		if rule, ok := autoMergeRule(in); ok {
			gate.State, gate.Rule = core.GateAutoMerge, rule
		}
	}
	return gate
}

func requiredChecks(in GateInput) []string {
	req := slices.Clone(in.Policy.RequiredChecks)
	if p := in.Result.Protection; p != nil {
		req = append(req, p.RequiredChecks...)
	}
	slices.Sort(req)
	return slices.Compact(req)
}

func checkReasons(in GateInput) []string {
	var why []string
	byName := map[string]core.CheckState{}
	for _, c := range in.Result.Checks {
		byName[c.Name] = c.State
		switch c.State {
		case core.CheckFailed:
			why = append(why, "check "+c.Name+" failed")
		case core.CheckPending:
			why = append(why, "check "+c.Name+" is still running")
		}
	}
	for _, name := range requiredChecks(in) {
		if _, ok := byName[name]; !ok {
			why = append(why, "required check "+name+" has not reported")
		}
	}
	return why
}

// activeVerdicts keeps each person's latest standing verdict, as an SCM does: a comment does
// not replace one, and a new commit does not clear a change request.
func activeVerdicts(reviews []core.Review) map[string]core.Review {
	out := map[string]core.Review{}
	for _, r := range reviews {
		if r.Author.Type != core.ActorHuman || r.Verdict == core.VerdictComment {
			continue
		}
		if r.Dismissed {
			delete(out, r.Author.Login)
			continue
		}
		out[r.Author.Login] = r
	}
	return out
}

func reviewReasons(in GateInput, sha string) []string {
	var why []string
	active := activeVerdicts(in.Result.Reviews)
	approvals := 0
	for _, login := range slices.Sorted(maps.Keys(active)) {
		r := active[login]
		switch {
		case r.Verdict == core.VerdictChangesRequested && r.Permission >= in.MinPerm:
			why = append(why, "@"+login+" requested changes, and only they can clear that")
		case countsAsApproval(r, in, sha):
			approvals++
		}
	}
	if need := in.Policy.MinHumanApprovals; approvals < need {
		why = append(why, fmt.Sprintf("it has %d of %d approvals by people on this commit",
			approvals, need))
	}
	open := 0
	for _, f := range in.Task.Findings {
		// A remark outside a thread has nothing to resolve: a change request stands through its
		// verdict above, and a general comment holds nothing up.
		if f.ThreadID == "" || f.Author.Type != core.ActorHuman {
			continue
		}
		switch f.Status {
		case core.FindingResolved, core.FindingStale, core.FindingSuperseded:
		default:
			open++
		}
	}
	if open > 0 {
		why = append(why, unresolvedRemarks(open))
	}
	return why
}

func unresolvedRemarks(n int) string {
	if n == 1 {
		return "1 remark by a person is unresolved"
	}
	return fmt.Sprintf("%d remarks by people are unresolved", n)
}

func countsAsApproval(r core.Review, in GateInput, sha string) bool {
	if r.Verdict != core.VerdictApprove || r.CommitSHA != sha || r.Permission < in.MinPerm {
		return false
	}
	if r.Author.Login == in.Result.PullRequest.Author.Login {
		return false
	}
	return len(in.Policy.Approvers) == 0 || slices.Contains(in.Policy.Approvers, r.Author.Login)
}

func protectionReasons(in GateInput) []string {
	if !in.Policy.RequireBranchProtection {
		return nil
	}
	if p := in.Result.Protection; p == nil || !p.Enabled {
		return []string{"the base branch is not known to be protected"}
	}
	return nil
}

func sensitiveCategories(files []string, p MergePolicy) []string {
	var out []string
	for _, category := range slices.Sorted(maps.Keys(p.SensitivePaths)) {
		if slices.ContainsFunc(files, func(f string) bool {
			return matchesAny(f,
				p.SensitivePaths[category])
		}) {
			out = append(out, category)
		}
	}
	return out
}

// autoMergeRule finds a rule that expressly covers this whole change. No gate gets here
// without a person's approval on this commit: MinHumanApprovals could be configured away, so
// the rule asks again, and an empty list of checks proves nothing either.
func autoMergeRule(in GateInput) (string, bool) {
	if !in.Result.CanMerge || len(in.Result.Files) == 0 || len(in.Result.Checks) == 0 {
		return "", false
	}
	floor := in
	floor.Policy.MinHumanApprovals = max(in.Policy.MinHumanApprovals, 1)
	if len(reviewReasons(floor, in.Task.HeadSHA)) > 0 {
		return "", false
	}
	for _, r := range in.Policy.AutoMerge {
		if r.MaxFiles > 0 && len(in.Result.Files) > r.MaxFiles {
			continue
		}
		if !slices.ContainsFunc(in.Result.Files, func(f string) bool {
			return !matchesAny(f,
				r.Paths)
		}) {
			return r.Name, true
		}
	}
	return "", false
}

func matchesAny(file string, patterns []string) bool {
	return slices.ContainsFunc(patterns, func(p string) bool { return matchPath(p, file) })
}

// matchPath matches a slash-separated path where ** spans any number of directories, and a
// leading **/ also matches at the root.
func matchPath(pattern, file string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(strings.ToLower(file), "/"))
}

func matchSegments(pat, file []string) bool {
	if len(pat) == 0 {
		return len(file) == 0
	}
	if pat[0] == "**" {
		if len(pat) == 1 {
			return true
		}
		for i := range len(file) + 1 {
			if matchSegments(pat[1:], file[i:]) {
				return true
			}
		}
		return false
	}
	if len(file) == 0 {
		return false
	}
	ok, err := path.Match(strings.ToLower(pat[0]), file[0])
	return err == nil && ok && matchSegments(pat[1:], file[1:])
}
