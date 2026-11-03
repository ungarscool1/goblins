package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/workflow"
)

// heldKeys lists the limit keys nothing may start on. Failing to read them must not stop
// work on every other account, so it only means trying and finding out.
func (s *Scheduler) heldKeys(ctx context.Context) map[string]bool {
	holds, err := s.Store.ActiveHolds(ctx, s.Now())
	if err != nil {
		s.Log.Warn("cannot read limit holds", "err", err)
		return nil
	}
	held := make(map[string]bool, len(holds))
	for _, h := range holds {
		held[h.LimitKey] = true
	}
	return held
}

// holdFor is how long an account rests after a failure. It is deliberately not the task's own
// pause, which grows: after this, one job tries again, and failing puts the hold back.
func (s *Scheduler) holdFor(kind core.ErrorKind, err error, now time.Time) (time.Time, bool) {
	var wait time.Duration
	switch kind {
	case core.KindQuotaExhausted:
		wait = s.Cfg.QuotaHold
	case core.KindRateLimited:
		wait = s.Cfg.RateLimitHold
	case core.KindAuthRequired:
		// Nothing a login check costs, so a person logging in is noticed soon.
		return now.Add(s.Cfg.AuthHold), true
	default:
		return time.Time{}, false
	}
	if at := core.RetryAtOf(err); at.After(now) {
		return at, true
	}
	return now.Add(wait), true
}

// updateHold keeps an account's other jobs from wasting a run on a limit one job just met,
// while jobs on any other account carry on.
func (s *Scheduler) updateHold(ctx context.Context, log *slog.Logger, job core.Job, err error,
	now time.Time) {
	if job.LimitKey == "" {
		return
	}
	if err == nil {
		if rerr := s.Store.ReleaseLimit(ctx, job.LimitKey); rerr != nil {
			log.Warn("cannot release a limit hold", "err", rerr)
		}
		return
	}
	kind := core.KindOf(err)
	until, ok := s.holdFor(kind, err, now)
	if !ok {
		return
	}
	herr := s.Store.HoldLimit(ctx, core.LimitHold{LimitKey: job.LimitKey, Until: until,
		Reason: string(kind)})
	if herr != nil {
		log.Warn("cannot hold a limit", "err", herr)
	}
}

func (s *Scheduler) releaseHoldJob(ctx context.Context, jc JobContext) (any, error) {
	var p workflow.ReleaseHoldPayload
	if err := json.Unmarshal(jc.Job.Payload, &p); err != nil {
		return nil, core.Wrap(core.KindInvalid, "scheduler.release_hold", err)
	}
	return struct{}{}, s.Store.ReleaseLimit(ctx, p.LimitKey)
}
