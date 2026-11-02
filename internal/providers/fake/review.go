package fake

import (
	"context"
	"fmt"
	"slices"

	"github.com/ungarscool1/goblins/internal/core"
)

// SubmitReview seeds a review as someone other than the integration.
func (w *World) SubmitReview(repo, number string, r core.Review) core.Review {
	w.mu.Lock()
	defer w.mu.Unlock()
	if r.ID == "" {
		r.ID = "rev-" + w.next()
	}
	w.submitted[repo+"#"+number] = append(w.submitted[repo+"#"+number], r)
	return r
}

// DismissReview marks a review as dismissed.
func (w *World) DismissReview(repo, number, id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, r := range w.submitted[repo+"#"+number] {
		if r.ID == id {
			w.submitted[repo+"#"+number][i].Dismissed = true
		}
	}
}

// AddThread seeds a review remark as someone other than the integration.
func (w *World) AddThread(repo, number string, t core.ReviewThread) core.ReviewThread {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t.ID == "" {
		t.ID = "thread-" + w.next()
	}
	if t.ThreadID == "" {
		t.ThreadID = t.ID
	}
	w.threads[repo+"#"+number] = append(w.threads[repo+"#"+number], t)
	return t
}

// SetThread changes a seeded remark.
func (w *World) SetThread(repo, number, id string, change func(*core.ReviewThread)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := range w.threads[repo+"#"+number] {
		if w.threads[repo+"#"+number][i].ID == id {
			change(&w.threads[repo+"#"+number][i])
		}
	}
}

// SetFiles seeds what a pull request changes.
func (w *World) SetFiles(repo, number string, files ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.files[repo+"#"+number] = files
}

// SetProtection seeds what the SCM enforces on a branch.
func (w *World) SetProtection(repo, branch string, p core.BranchProtection) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.protection[repo+"@"+branch] = p
}

// SetPullRequest changes a pull request as someone else would.
func (w *World) SetPullRequest(repo, number string, change func(*core.PullRequest)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	change(w.prs[repo+"#"+number])
}

// Merges lists the commits merged through the SCM.
func (w *World) Merges() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.merges)
}

// ListReviews implements core.SourceControl.
func (s *SCM) ListReviews(_ context.Context, repo core.RepoRef, number string) ([]core.Review,
	error) {
	s.W.mu.Lock()
	defer s.W.mu.Unlock()
	out := slices.Clone(s.W.submitted[repo.Slug+"#"+number])
	for i, r := range s.W.reviews[repo.Slug+"#"+number] {
		out = append(out, core.Review{ID: fmt.Sprintf("own-%d", i), Author: s.W.Bot,
			Verdict: r.Verdict,
			Body:    r.Body, CommitSHA: r.CommitSHA, Own: true})
	}
	if out == nil {
		out = []core.Review{}
	}
	return out, nil
}

// ChangedFiles implements core.SourceControl.
func (s *SCM) ChangedFiles(_ context.Context, repo core.RepoRef, number string) ([]string, error) {
	s.W.mu.Lock()
	defer s.W.mu.Unlock()
	return slices.Clone(s.W.files[repo.Slug+"#"+number]), nil
}

// BranchProtection implements core.SourceControl.
func (s *SCM) BranchProtection(_ context.Context, repo core.RepoRef,
	branch string) (core.BranchProtection, error) {
	if !s.Info().Has(core.CapSCMBranchProtection) {
		return core.BranchProtection{}, core.Unsupported(s.Info(), core.CapSCMBranchProtection)
	}
	s.W.mu.Lock()
	defer s.W.mu.Unlock()
	return s.W.protection[repo.Slug+"@"+branch], nil
}

// MergePullRequest implements core.SourceControl.
func (s *SCM) MergePullRequest(_ context.Context, repo core.RepoRef, number, sha string) error {
	if !s.Info().Has(core.CapSCMMerge) {
		return core.Unsupported(s.Info(), core.CapSCMMerge)
	}
	s.W.mu.Lock()
	defer s.W.mu.Unlock()
	pr, ok := s.W.prs[repo.Slug+"#"+number]
	if !ok {
		return core.Errorf(core.KindNotFound, "fake.merge", "pull request %s not found", number)
	}
	if pr.HeadSHA != sha {
		return core.Errorf(core.KindConflict, "fake.merge", "head moved")
	}
	pr.Open, pr.Merged = false, true
	s.W.merges = append(s.W.merges, sha)
	return nil
}
