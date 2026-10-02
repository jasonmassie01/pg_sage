package earned

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

var _ policy.AutonomyDeadlineRecorder = (*Limiter)(nil)

// RecordDeadlineOverride records a mandatory deadline action the gate let
// run without the ledger level (coordinator decision 2026-10-02): once per
// pair, target set and deadline, however often the gate re-authorizes it.
// A failure is logged and retried on the next authorization; it never
// blocks the action.
func (l *Limiter) RecordDeadlineOverride(ctx context.Context, req policy.ActionRequest,
	d policy.Decision) {
	if req.Deadline == nil {
		return
	}
	f, c := Family(strings.TrimSpace(req.IncidentFamily)), ClassFor(req)
	key, fresh := l.claimOverride(f, c, req)
	if !fresh {
		return
	}
	if err := l.appendOverride(ctx, f, c, req, d); err != nil {
		l.svc.cfg.logf("autonomy: record deadline override for %s/%s on %s: %v", f, c,
			l.b.Database, err)
		l.mu.Lock()
		delete(l.overrides, key)
		l.mu.Unlock()
	}
}

// claimOverride marks the override recorded; fresh is false when it was
// already. Entries are dropped once their deadline has passed.
func (l *Limiter) claimOverride(f Family, c ActionClass, req policy.ActionRequest) (string,
	bool) {
	targets := append([]string(nil), req.TargetObjs...)
	sort.Strings(targets)
	key := fmt.Sprintf("%s|%s|%s|%d", f, c, strings.Join(targets, ","),
		req.Deadline.HardAt.UnixNano())
	now := l.svc.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, hardAt := range l.overrides {
		if !hardAt.After(now) {
			delete(l.overrides, k)
		}
	}
	if _, seen := l.overrides[key]; seen {
		return key, false
	}
	l.overrides[key] = req.Deadline.HardAt
	return key, true
}

func (l *Limiter) appendOverride(ctx context.Context, f Family, c ActionClass,
	req policy.ActionRequest, d policy.Decision) error {
	st, err := l.svc.Granted(ctx, f, c)
	if err != nil {
		return err
	}
	evidence, err := json.Marshal(map[string]any{"targets": req.TargetObjs,
		"deadline_kind": req.Deadline.Kind, "urgency": req.Deadline.Urgency,
		"hard_at": req.Deadline.HardAt.UTC(), "gate_reason": d.Reason,
		"granted": st.Level})
	if err != nil {
		return fmt.Errorf("encode deadline override: %w", err)
	}
	reason := fmt.Sprintf("mandatory %s deadline (%s, hard at %s) ran without the "+
		"ledger level (granted %s); gate reason %s", req.Deadline.Kind,
		req.Deadline.Urgency, req.Deadline.HardAt.UTC().Format(time.RFC3339), st.Level,
		d.Reason)
	return l.svc.store.appendEvent(ctx, l.svc.store.pool, Event{Family: f, Class: c,
		Type: EventDeadlineOverride, Actor: ActorPgSage, Reason: truncate(reason, 2000),
		Database: l.b.Database, Evidence: evidence, At: l.svc.now()})
}
