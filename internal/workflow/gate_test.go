package workflow

import (
	"slices"
	"strings"
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
)

const head = "head1"

var (
	alice  = core.Actor{Login: "alice", Type: core.ActorHuman}
	bob    = core.Actor{Login: "bob", Type: core.ActorHuman}
	robot  = core.Actor{Login: "robot[bot]", Type: core.ActorBot}
	author = core.Actor{Login: "goblins[bot]", Type: core.ActorBot}
)

func approval(a core.Actor) core.Review {
	return core.Review{ID: a.Login, Author: a, Verdict: core.VerdictApprove, CommitSHA: head, Permission: core.PermissionWrite}
}

func gateInput() GateInput {
	return GateInput{
		Task: core.Task{Kind: core.TaskIssue, HeadSHA: head, ValidatedSHA: head, DesignRevision: 1, Approval: &core.Approval{Revision: 1}},
		Result: ReconcileResult{
			PullRequest: core.PullRequest{Author: author, HeadSHA: head, Open: true},
			Reviews:     []core.Review{approval(alice)},
			Checks:      []core.StatusCheck{{Name: "test", State: core.CheckPassed}},
			Files:       []string{"docs/guide.md"},
			CanMerge:    true,
		},
		Policy: DefaultMergePolicy(), MinPerm: core.PermissionWrite,
	}
}

func TestGate(t *testing.T) {
	cases := []struct {
		name   string
		change func(*GateInput)
		state  core.GateState
		reason string
	}{
		{"a clean change waits for a person to merge", func(*GateInput) {}, core.GateReady, ""},
		{"draft", func(i *GateInput) { i.Result.PullRequest.Draft = true }, core.GateWaiting, "draft"},
		{"no approval", func(i *GateInput) { i.Result.Reviews = nil }, core.GateWaiting, "0 of 1 approvals"},
		{"an approval on an earlier commit does not count", func(i *GateInput) { i.Result.Reviews[0].CommitSHA = "old" }, core.GateWaiting, "0 of 1"},
		{"an approval by automation does not count", func(i *GateInput) { i.Result.Reviews = []core.Review{approval(robot)} }, core.GateWaiting, "0 of 1"},
		{"an approval without write access does not count", func(i *GateInput) { i.Result.Reviews[0].Permission = core.PermissionRead }, core.GateWaiting, "0 of 1"},
		{"the author cannot approve", func(i *GateInput) { i.Result.PullRequest.Author = alice }, core.GateWaiting, "0 of 1"},
		{"only listed approvers count", func(i *GateInput) { i.Policy.Approvers = []string{"bob"} }, core.GateWaiting, "0 of 1"},
		{"listed approver", func(i *GateInput) { i.Policy.Approvers = []string{"alice"} }, core.GateReady, ""},
		{"a dismissed approval is gone", func(i *GateInput) { i.Result.Reviews[0].Dismissed = true }, core.GateWaiting, "0 of 1"},
		{"a person's change request stands", func(i *GateInput) {
			i.Result.Reviews = append(i.Result.Reviews, core.Review{Author: bob, Verdict: core.VerdictChangesRequested, CommitSHA: "old", Permission: core.PermissionWrite})
		}, core.GateWaiting, "@bob requested changes"},
		{"a new verdict by the same person replaces it", func(i *GateInput) {
			i.Result.Reviews = append([]core.Review{{Author: alice, Verdict: core.VerdictChangesRequested, CommitSHA: "old"}}, i.Result.Reviews...)
		}, core.GateReady, ""},
		{"a comment does not replace a verdict", func(i *GateInput) {
			i.Result.Reviews = []core.Review{{Author: bob, Verdict: core.VerdictChangesRequested, CommitSHA: "old", Permission: core.PermissionWrite},
				{Author: bob, Verdict: core.VerdictComment, CommitSHA: head}, approval(alice)}
		}, core.GateWaiting, "@bob requested changes"},
		{"a dismissed change request is gone", func(i *GateInput) {
			i.Result.Reviews = append(i.Result.Reviews, core.Review{Author: bob, Verdict: core.VerdictChangesRequested, Dismissed: true})
		}, core.GateReady, ""},
		{"a change request by someone without access holds nothing up", func(i *GateInput) {
			i.Result.Reviews = append(i.Result.Reviews, core.Review{Author: bob, Verdict: core.VerdictChangesRequested, CommitSHA: head, Permission: core.PermissionRead})
		}, core.GateReady, ""},
		{"a remark outside a thread has nothing to resolve", func(i *GateInput) {
			i.Task.Findings = []core.ReviewFinding{{ID: "comment:1", Author: bob, Status: core.FindingOpen}}
		}, core.GateReady, ""},
		{"a failed check", func(i *GateInput) { i.Result.Checks[0].State = core.CheckFailed }, core.GateWaiting, "check test failed"},
		{"a running check", func(i *GateInput) { i.Result.Checks[0].State = core.CheckPending }, core.GateWaiting, "still running"},
		{"a required check missing", func(i *GateInput) { i.Policy.RequiredChecks = []string{"security"} }, core.GateWaiting, "required check security"},
		{"protection requires a check", func(i *GateInput) {
			i.Result.Protection = &core.BranchProtection{Enabled: true, RequiredChecks: []string{"lint"}}
		}, core.GateWaiting, "required check lint"},
		{"protection unknown", func(i *GateInput) { i.Policy.RequireBranchProtection = true }, core.GateWaiting, "not known to be protected"},
		{"unprotected", func(i *GateInput) {
			i.Policy.RequireBranchProtection = true
			i.Result.Protection = &core.BranchProtection{}
		}, core.GateWaiting, "not known to be protected"},
		{"protected", func(i *GateInput) {
			i.Policy.RequireBranchProtection = true
			i.Result.Protection = &core.BranchProtection{Enabled: true}
		}, core.GateReady, ""},
		{"a voided design", func(i *GateInput) { i.Task.Approval = nil }, core.GateWaiting, "not approved"},
		{"an unvalidated commit", func(i *GateInput) { i.Task.ValidatedSHA = "old" }, core.GateWaiting, "validation"},
		{"an open remark by a person", func(i *GateInput) {
			i.Task.Findings = []core.ReviewFinding{{ThreadID: "t", Author: bob, Status: core.FindingOpen}}
		}, core.GateWaiting, "1 remark by"},
		{"a remark sent to the implementer is still not resolved", func(i *GateInput) {
			i.Task.Findings = []core.ReviewFinding{{ThreadID: "t", Author: bob, Status: core.FindingAddressed}}
		}, core.GateWaiting, "1 remark by"},
		{"a remark on lines that moved stays open until it is resolved", func(i *GateInput) {
			i.Task.Findings = []core.ReviewFinding{{ThreadID: "u", Author: bob, Status: core.FindingStale}}
		}, core.GateWaiting, "1 remark by"},
		{"resolved remarks do not count", func(i *GateInput) {
			i.Task.Findings = []core.ReviewFinding{{ThreadID: "t", Author: bob, Status: core.FindingResolved}}
		}, core.GateReady, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := gateInput()
			tc.change(&in)
			g := EvaluateGate(in)
			if g.State != tc.state {
				t.Fatalf("state = %s, reasons = %v", g.State, g.Reasons)
			}
			if !strings.Contains(strings.Join(g.Reasons, "\n"), tc.reason) {
				t.Fatalf("reasons = %v", g.Reasons)
			}
		})
	}
}

func TestSensitiveChangesAlwaysStayWithAPerson(t *testing.T) {
	for file, category := range map[string]string{
		"internal/auth/token.go":  "authentication",
		"pkg/oauth_client.go":     "authentication",
		"src/crypto/aes.go":       "cryptography",
		"certs/server.pem":        "cryptography",
		"lib/network/dial.go":     "network",
		"proxy.conf":              "network",
		".ci/workflows/build.yml": "automation",
		"deploy/app.yaml":         "automation",
	} {
		in := gateInput()
		in.Result.Files = []string{"docs/guide.md", file}
		in.Policy.AutoMerge = []AutoMergeRule{{Name: "anything", Paths: []string{"**"}}}
		g := EvaluateGate(in)
		if g.State != core.GateReady || !slices.Contains(g.Categories, category) {
			t.Errorf("%s: gate = %+v, want it ready for a person, under %s", file, g, category)
		}
	}
}

func TestAutoMergeNeedsAnExpressRuleAndAPerson(t *testing.T) {
	docs := []AutoMergeRule{{Name: "docs", Paths: []string{"docs/**"}, MaxFiles: 2}}
	cases := []struct {
		name   string
		change func(*GateInput)
		want   core.GateState
	}{
		{"on by rule", func(*GateInput) {}, core.GateAutoMerge},
		{"off by default", func(i *GateInput) { i.Policy.AutoMerge = nil }, core.GateReady},
		{"a file outside the rule", func(i *GateInput) { i.Result.Files = []string{"docs/a.md", "main.go"} }, core.GateReady},
		{"too many files", func(i *GateInput) { i.Result.Files = []string{"docs/a", "docs/b", "docs/c"} }, core.GateReady},
		{"no idea what it changes", func(i *GateInput) { i.Result.Files = nil }, core.GateReady},
		{"nothing checked it", func(i *GateInput) { i.Result.Checks = nil }, core.GateReady},
		{"the SCM cannot merge", func(i *GateInput) { i.Result.CanMerge = false }, core.GateReady},
		{"tests and an AI review alone cannot merge", func(i *GateInput) {
			i.Policy.MinHumanApprovals = 0
			i.Result.Reviews = []core.Review{approval(robot)}
		}, core.GateReady},
		{"no approval of this commit", func(i *GateInput) { i.Result.Reviews[0].CommitSHA = "old" }, core.GateWaiting},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := gateInput()
			in.Policy.AutoMerge = docs
			tc.change(&in)
			if g := EvaluateGate(in); g.State != tc.want {
				t.Fatalf("gate = %+v, want %s", g, tc.want)
			}
		})
	}
}

func TestMatchPath(t *testing.T) {
	for _, tc := range []struct {
		pattern, file string
		want          bool
	}{
		{"docs/**", "docs/a/b.md", true}, {"docs/**", "src/docs/a.md", false}, {"**/auth/**", "auth/x.go", true},
		{"**/auth/**", "a/auth/b/x.go", true}, {"**/*auth*", "pkg/OAuth.go", true}, {"Dockerfile", "Dockerfile", true},
		{"**/Dockerfile", "a/b/Dockerfile", true}, {"**/*.pem", "a.pem", true}, {"*.md", "a/b.md", false},
	} {
		if got := matchPath(tc.pattern, tc.file); got != tc.want {
			t.Errorf("%s on %s = %v", tc.pattern, tc.file, got)
		}
	}
}

func TestMergeFindings(t *testing.T) {
	seen := []core.ReviewFinding{{ID: "a", Body: "x", Status: core.FindingOpen}}
	known := mergeFindings(nil, seen)
	known[0].Status = core.FindingAddressed
	again := mergeFindings(known, seen)
	if len(again) != 1 || again[0].Status != core.FindingAddressed {
		t.Fatalf("a replay changed what happened to a finding: %+v", again)
	}
	stale := mergeFindings(again, []core.ReviewFinding{{ID: "a", Status: core.FindingStale}})
	if stale[0].Status != core.FindingStale {
		t.Fatalf("not stale: %+v", stale)
	}
	resolved := mergeFindings(stale, []core.ReviewFinding{{ID: "a", Status: core.FindingResolved}})
	reopened := mergeFindings(resolved, []core.ReviewFinding{{ID: "a", Status: core.FindingOpen}})
	if reopened[0].Status != core.FindingOpen {
		t.Fatalf("not reopened: %+v", reopened)
	}
	var many []core.ReviewFinding
	for i := range maxFindings + 10 {
		many = append(many, core.ReviewFinding{ID: string(rune(i + 1000))})
	}
	if got := mergeFindings(nil, many); len(got) != maxFindings {
		t.Fatalf("unbounded: %d", len(got))
	}
}

func TestUnresolvedRemarksReadNaturally(t *testing.T) {
	if got := unresolvedRemarks(1); got != "1 remark by a person is unresolved" {
		t.Error(got)
	}
	if got := unresolvedRemarks(2); got != "2 remarks by people are unresolved" {
		t.Error(got)
	}
}
