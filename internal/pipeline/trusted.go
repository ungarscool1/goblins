package pipeline

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/runtime/workspace"
	"github.com/ungarscool1/goblins/internal/scheduler"
)

const maxTrustedListed = 10

var unsafeFileName = regexp.MustCompile(`[^\w./-]`)

// trustedResult is what checks take from the base commit rather than from the change.
type trustedResult struct {
	manifest []byte
	ignored  []string
}

// applyTrusted puts the base commit's version of trusted files in the checks' copy, so a change
// cannot pass by weakening them, and reads the manifest from there for the same reason.
func (p *Pipeline) applyTrusted(ctx context.Context, jc scheduler.JobContext, pr *Project,
	scratch string) (trustedResult, error) {
	if pr.ValidationManifest == "" && len(pr.TrustedPaths) == 0 {
		return trustedResult{}, nil
	}
	spec, err := workspace.TrustedSpec(workloadName(jc, "trusted"), gitImage(pr), scratch,
		p.labels(jc), jc.Task.BaseSHA, pr.ValidationManifest, pr.TrustedPaths)
	if err != nil {
		return trustedResult{}, err
	}
	st, _, err := runStep(ctx, pr.Implementer.Runtime, spec)
	if err != nil {
		return trustedResult{}, err
	}
	manifest, err := base64.StdEncoding.DecodeString(st.Manifest)
	if err != nil {
		return trustedResult{}, core.Wrap(core.KindInternal, "pipeline.trusted", err)
	}
	changed, err := base64.StdEncoding.DecodeString(st.TrustedChanged)
	if err != nil {
		return trustedResult{}, core.Wrap(core.KindInternal, "pipeline.trusted", err)
	}
	res := trustedResult{}
	if len(manifest) > 0 {
		res.manifest = manifest
	}
	for name := range strings.SplitSeq(string(changed), "\n") {
		if name != "" {
			res.ignored = append(res.ignored, name)
		}
	}
	if len(res.ignored) > 0 {
		jc.Log("system", "changes to trusted files were left out of the checks: "+string(changed))
	}
	return res, nil
}

// note tells people what the checks did not use. File names come from the change and end up
// on the ticket, so they are reduced to what a path needs.
func (r trustedResult) note() string {
	if len(r.ignored) == 0 {
		return ""
	}
	names := make([]string, 0, maxTrustedListed)
	for _, n := range r.ignored[:min(len(r.ignored), maxTrustedListed)] {
		names = append(names, "`"+unsafeFileName.ReplaceAllString(n, "?")+"`")
	}
	more := ""
	if n := len(r.ignored) - len(names); n > 0 {
		more = fmt.Sprintf(" and %d more", n)
	}
	return ". This change also edits trusted files, and the checks used the base commit's " +
		"version of them: " + strings.Join(names, ", ") + more + "."
}

// reusedReport says what a published commit passed before publishing, as it runs nothing again.
func reusedReport(task core.Task) core.ValidationReport {
	rep := core.ValidationReport{Status: core.ValidationPassed,
		Summary: "this exact commit already passed before publishing"}
	if prior := task.ValidatedReport; prior != nil {
		rep.Summary += ": " + prior.Summary
		rep.Checks = prior.Checks
	}
	return rep
}
