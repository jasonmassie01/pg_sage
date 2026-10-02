package action

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/store"
)

// The action service against real PostgreSQL: durable proposals linked to
// their investigation, exactly one approval item per proposal, execution
// only after approval and only through the executor, identity rechecked
// right before the signal, every step on the investigation's hash chain.

func TestActionProposeCreatesOneEvidenceMatchedProposal(t *testing.T) {
	h := newActionHarness(t, nil)
	p, err := h.actions.Propose(h.ctx, h.inv.ID, "user:1")
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if p.State != ProposalProposed || p.Class != ActionCancelBackend ||
		p.InvestigationID != h.inv.ID || p.Node != "ddl_lock_queue" ||
		p.Family != "lock_blocking" || p.SQL != "SELECT pg_cancel_backend(5151)" {
		t.Fatalf("proposal = %+v", p)
	}
	tg := p.Target
	if tg == nil || tg.PID != testTargetPID || !tg.BackendStart.Equal(deriveStart) ||
		tg.Database != "orders" || tg.User != "app" || tg.QueryHash != testQueryHash ||
		tg.QueryID != 77 || tg.ObservedAt.IsZero() {
		t.Fatalf("target = %+v", tg)
	}
	ev, _ := h.st.Evidence(h.ctx, h.inv.Scope, h.inv.ID)
	if len(p.EvidenceIDs) == 0 || !containsEvidence(ev, p.EvidenceIDs[0], "lock_graph") {
		t.Fatalf("evidence ids %v do not cite the investigation's lock graph", p.EvidenceIDs)
	}
	if err := p.Contract.Validate(); err != nil ||
		p.Contract.Reversibility != executor.ReversibilityMitigationOnly {
		t.Fatalf("contract = %+v (%v)", p.Contract, err)
	}
	if p.Policy.Decision != executor.PolicyDecisionExecute || p.Baseline.Waiting != 2 {
		t.Fatalf("policy %+v baseline %+v", p.Policy, p.Baseline)
	}
	again, err := h.actions.Propose(h.ctx, h.inv.ID, "user:2")
	if err != nil || again.ID != p.ID {
		t.Fatalf("second propose = %s (%v), want the same proposal %s", again.ID, err, p.ID)
	}
	if got := countOf(h.eventTypes(t), "action_proposed"); got != 1 {
		t.Fatalf("action_proposed events = %d, want 1", got)
	}
	if h.cancel.callCount() != 0 || h.queueRows(t, p.ID) != 0 {
		t.Fatal("proposing executed or queued something")
	}
}

func containsEvidence(ev []sre.Evidence, id sre.UUID, probe string) bool {
	for _, e := range ev {
		if e.ID == id && e.ProbeID == probe {
			return true
		}
	}
	return false
}

// CHECK-02: an idle-in-transaction holder is never misrepresented as
// fixed by cancellation.
func TestActionProposeRecordsIneligibleRoots(t *testing.T) {
	h := newActionHarness(t, idleChainRunner())
	p, err := h.actions.Propose(h.ctx, h.inv.ID, "user:1")
	if err != nil || p.State != ProposalIneligible || p.Reason != ReasonIdleInTransaction ||
		!strings.Contains(p.Detail, "pg_cancel_backend") {
		t.Fatalf("idle holder proposal = %+v, %v", p, err)
	}
	if _, err := h.actions.RequestExecution(h.ctx, p.ID, "user:1"); !errors.Is(err,
		ErrProposalState) {
		t.Fatalf("requesting an ineligible proposal = %v, want ErrProposalState", err)
	}
	if h.queueRows(t, p.ID) != 0 || h.cancel.callCount() != 0 {
		t.Fatal("an ineligible proposal reached the queue or the executor")
	}
}

func TestActionProposeChecksTheLiveTarget(t *testing.T) {
	for name, tc := range map[string]struct {
		res  probes.Result
		cfg  func(*ActionConfig)
		want ActionReason
	}{
		"gone":       {res: rows(probes.SignalTarget), want: ReasonTargetGone},
		"new query":  {res: targetWith("query_id", int64(78)), want: ReasonTargetChanged},
		"idle now":   {res: targetWith("state", "idle"), want: ReasonTargetNotActive},
		"no waiters": {res: targetWith("blocking", int64(0)), want: ReasonNotBlocking},
		"replica":    {res: targetWith("in_recovery", true), want: ReasonReplica},
		"walsender": {res: targetWith("backend_type", "walsender"),
			want: ReasonProtected},
		"pg_dump": {res: targetProbeRow(func(r probes.Row) {
			r["protected_application"] = true
		}), want: ReasonProtected},
		"other database": {res: targetProbeRow(func(r probes.Row) {
			r["in_current_database"] = false
		}), want: ReasonOtherDatabase},
		"operator protected role": {res: targetProbeRow(),
			cfg: func(c *ActionConfig) { c.ProtectedRoles = []string{"app"} }, want: ReasonProtected},
		"probe refused": {res: probes.Result{ProbeID: probes.SignalTarget, Version: "v1",
			Status: probes.StatusNoPrivilege, Reason: "permission_denied"},
			want: ReasonTargetProbeFailed},
	} {
		t.Run(name, func(t *testing.T) {
			var mutate []func(*ActionConfig)
			if tc.cfg != nil {
				mutate = append(mutate, tc.cfg)
			}
			h := newActionHarness(t, nil, mutate...)
			h.targets.script(probes.SignalTarget, tc.res)
			p, err := h.actions.Propose(h.ctx, h.inv.ID, "user:1")
			if err != nil || p.State != ProposalIneligible || p.Reason != tc.want {
				t.Fatalf("proposal = %s/%s (%v), want ineligible %s", p.State, p.Reason,
					err, tc.want)
			}
		})
	}
}

// CHECK-39 (service half): requesting execution creates exactly one
// approval item, whoever asks and however often, and executes nothing.
func TestActionRequestExecutionCreatesExactlyOneApprovalItem(t *testing.T) {
	h := newActionHarness(t, nil)
	p, err := h.actions.Propose(h.ctx, h.inv.ID, "user:1")
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	var wg sync.WaitGroup
	queueIDs := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := h.actions.RequestExecution(h.ctx, p.ID, "mcp:user:2")
			if err != nil {
				t.Errorf("concurrent request: %v", err)
				return
			}
			queueIDs <- got.QueueID
		}()
	}
	wg.Wait()
	close(queueIDs)
	first := 0
	for id := range queueIDs {
		if first == 0 {
			first = id
		}
		if id != first || id <= 0 {
			t.Fatalf("requests returned queue ids %d and %d", first, id)
		}
	}
	if n := h.queueRows(t, p.ID); n != 1 {
		t.Fatalf("approval items = %d, want exactly 1", n)
	}
	var status, actionType, risk, sql string
	var recommended *string
	var expires time.Time
	err = h.pool.QueryRow(h.ctx, `SELECT q.status, q.action_type, q.action_risk,
		q.proposed_sql, q.expires_at, f.recommended_sql
		FROM sage.action_queue q JOIN sage.findings f ON f.id = q.finding_id
		WHERE q.id = $1`, first).Scan(&status, &actionType, &risk, &sql, &expires,
		&recommended)
	if err != nil {
		t.Fatalf("queue row: %v", err)
	}
	if status != "pending" || actionType != "cancel_backend" || risk != "moderate" ||
		sql != "SELECT pg_cancel_backend(5151)" || recommended != nil ||
		time.Until(expires) > h.cfg.ApprovalTTL+time.Minute ||
		time.Until(expires) < h.cfg.ApprovalTTL-time.Minute {
		t.Fatalf("queue row = %s %s %s %q expires %s recommended %v", status, actionType,
			risk, sql, expires, recommended)
	}
	if h.notes.count() != 1 || h.cancel.callCount() != 0 {
		t.Fatalf("notifications %d, cancels %d; want 1 and 0", h.notes.count(),
			h.cancel.callCount())
	}
	if got := countOf(h.eventTypes(t), "action_requested"); got != 1 {
		t.Fatalf("action_requested events = %d, want 1", got)
	}
}

func TestActionRequestRefusedWhilePolicyWithholds(t *testing.T) {
	h := newActionHarness(t, nil)
	h.cancel.preview = executor.ActionPolicyDecision{Decision: executor.PolicyDecisionBlocked,
		BlockedReason: "observe_only", RiskTier: "moderate"}
	p, err := h.actions.Propose(h.ctx, h.inv.ID, "user:1")
	if err != nil || p.State != ProposalProposed || p.Policy.Reason != "observe_only" {
		t.Fatalf("proposal under observation trust = %+v, %v", p, err)
	}
	_, err = h.actions.RequestExecution(h.ctx, p.ID, "user:1")
	if !errors.Is(err, ErrPolicyBlocked) || !strings.Contains(err.Error(), "observe") {
		t.Fatalf("request = %v, want ErrPolicyBlocked naming the reason", err)
	}
	if h.queueRows(t, p.ID) != 0 || h.notes.count() != 0 {
		t.Fatal("a withheld proposal reached the approval queue")
	}
}

// CHECK-16: a metadata outage blocks the handoff explicitly.
func TestActionHandoffBlockedWhileMetadataIsDegraded(t *testing.T) {
	h := newActionHarness(t, nil)
	p := h.requested(t)
	action := h.approved(t, p, 3)
	h.coord.Durability().Observe(sre.ErrMetadataUnavailable)
	if _, err := h.actions.RequestExecution(h.ctx, p.ID, "user:1"); !errors.Is(err,
		ErrHandoffBlocked) {
		t.Fatalf("request while degraded = %v, want ErrHandoffBlocked", err)
	}
	if _, err := h.actions.RunApproved(h.ctx, action, 3); !errors.Is(err,
		ErrHandoffBlocked) {
		t.Fatalf("run while degraded = %v, want ErrHandoffBlocked", err)
	}
	if h.cancel.callCount() != 0 {
		t.Fatal("a degraded store let the cancel through")
	}
}

func TestActionRunApprovedSignalsTheFreshExactTarget(t *testing.T) {
	h := newActionHarness(t, nil)
	p := h.requested(t)
	action := h.approved(t, p, 3)
	before := time.Now()
	run, err := h.actions.RunApproved(h.ctx, action, 3)
	if err != nil {
		t.Fatalf("RunApproved: %v", err)
	}
	if run.ActionLogID != 4711 || run.VerificationStatus != "monitoring" ||
		h.cancel.callCount() != 1 {
		t.Fatalf("run = %+v, cancels = %d", run, h.cancel.callCount())
	}
	req := h.cancel.calls[0]
	want := executor.BackendIdentity{PID: testTargetPID, BackendStart: deriveStart,
		QueryStart: deriveStart.Add(time.Minute), Database: "orders", User: "app",
		QueryHash: testQueryHash, QueryID: 77}
	if !sameIdentity(req.Target, want) || req.ApprovedBy != 3 ||
		req.FindingID != p.FindingID || req.MaxEvidenceAge != 5*time.Second ||
		req.ObservedAt.Before(before) || req.IsReplica {
		t.Fatalf("cancel request = %+v", req)
	}
	if req.Evidence["proposal_id"] != string(p.ID) ||
		req.Evidence["investigation_id"] != string(h.inv.ID) {
		t.Fatalf("cancel evidence = %#v", req.Evidence)
	}
	got := h.proposal(t, p.ID)
	if got.State != ProposalExecuted || got.ActionLogID != 4711 || got.DecidedBy != 3 ||
		got.Recovery.State != RecoveryObserving ||
		got.Recovery.Attribution != AttributionSage || got.Recovery.Deadline.IsZero() {
		t.Fatalf("executed proposal = %+v", got)
	}
	types := h.eventTypes(t)
	for _, want := range []string{"action_decided", "action_recheck", "action_executed"} {
		if countOf(types, want) != 1 {
			t.Fatalf("events %v lack one %s", types, want)
		}
	}
}

func sameIdentity(a, b executor.BackendIdentity) bool {
	return a.PID == b.PID && a.BackendStart.Equal(b.BackendStart) &&
		a.QueryStart.Equal(b.QueryStart) && a.Database == b.Database &&
		a.User == b.User && a.QueryHash == b.QueryHash && a.QueryID == b.QueryID
}

// CHECK-19 and CHECK-23: a target that changed or went away is refused
// with the reason on the timeline, never signalled, and what happened is
// attributed to an external or natural change, then verified.
func TestActionRunApprovedRefusesAChangedOrMissingTarget(t *testing.T) {
	for name, tc := range map[string]struct {
		res  probes.Result
		want ActionReason
	}{
		"new query on the session": {targetProbeRow(func(r probes.Row) {
			r["query_start"] = deriveStart.Add(2 * time.Minute)
		}), ReasonTargetChanged},
		"other text": {targetProbeRow(func(r probes.Row) {
			r["query_hash"] = strings.Repeat("e", 64)
		}), ReasonTargetChanged},
		"gone":          {rows(probes.SignalTarget), ReasonTargetGone},
		"stopped block": {targetWith("blocking", int64(0)), ReasonNotBlocking},
	} {
		t.Run(name, func(t *testing.T) {
			h := newActionHarness(t, nil)
			p := h.requested(t)
			action := h.approved(t, p, 3)
			h.targets.script(probes.SignalTarget, tc.res)
			_, err := h.actions.RunApproved(h.ctx, action, 3)
			if !errors.Is(err, executor.ErrBackendEvidenceStale) {
				t.Fatalf("RunApproved = %v, want ErrBackendEvidenceStale", err)
			}
			got := h.proposal(t, p.ID)
			if got.State != ProposalRefused || got.Reason != tc.want ||
				h.cancel.callCount() != 0 {
				t.Fatalf("proposal %s/%s, cancels %d", got.State, got.Reason,
					h.cancel.callCount())
			}
			if got.Recovery.State != RecoveryObserving ||
				got.Recovery.Attribution != AttributionExternal {
				t.Fatalf("recovery = %+v, want observing, attributed external", got.Recovery)
			}
			if countOf(h.eventTypes(t), "action_refused") != 1 {
				t.Fatal("refusal not on the timeline")
			}
		})
	}
}

func TestActionRunApprovedRecordsExecutorOutcomes(t *testing.T) {
	withheld := &executor.WithheldError{Decision: executor.ActionPolicyDecision{
		Decision: executor.PolicyDecisionBlocked, BlockedReason: "emergency_stop"}}
	for name, tc := range map[string]struct {
		err    error
		state  ProposalState
		reason ActionReason
	}{
		"policy":  {withheld, ProposalRefused, ReasonPolicyWithheld},
		"stale":   {executor.ErrBackendEvidenceStale, ProposalRefused, ReasonEvidenceStale},
		"failure": {errors.New("permission denied to cancel"), ProposalFailed, ReasonExecutionError},
	} {
		t.Run(name, func(t *testing.T) {
			h := newActionHarness(t, nil)
			h.cancel.err = tc.err
			p := h.requested(t)
			_, err := h.actions.RunApproved(h.ctx, h.approved(t, p, 3), 3)
			if !errors.Is(err, tc.err) {
				t.Fatalf("RunApproved = %v, want %v", err, tc.err)
			}
			got := h.proposal(t, p.ID)
			if got.State != tc.state || got.Reason != tc.reason || got.ActionLogID != 0 ||
				got.Recovery.State != RecoveryNone {
				t.Fatalf("proposal = %s/%s log %d recovery %s", got.State, got.Reason,
					got.ActionLogID, got.Recovery.State)
			}
			if name == "policy" && !strings.Contains(got.Detail, "emergency") {
				t.Fatalf("policy refusal detail = %q", got.Detail)
			}
		})
	}
}

// CHECK-20: concurrent runs of one approved item signal once.
func TestActionRunApprovedRunsOnce(t *testing.T) {
	h := newActionHarness(t, nil)
	p := h.requested(t)
	action := h.approved(t, p, 3)
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.actions.RunApproved(h.ctx, action, 3)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	ok := 0
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrProposalState):
			t.Fatalf("losing run = %v, want ErrProposalState", err)
		}
	}
	if ok != 1 || h.cancel.callCount() != 1 {
		t.Fatalf("successful runs %d, cancels %d; want 1 and 1", ok, h.cancel.callCount())
	}
	if _, err := h.actions.RunApproved(h.ctx, action, 3); !errors.Is(err, ErrProposalState) {
		t.Fatalf("a replayed approval = %v, want ErrProposalState", err)
	}
}

func TestActionOwnershipAndLinkage(t *testing.T) {
	h := newActionHarness(t, nil)
	p := h.requested(t)
	action := h.approved(t, p, 3)
	if !h.actions.Owns(action) {
		t.Fatal("the service does not own its approval item")
	}
	for name, mutate := range map[string]func(*store.QueuedAction){
		"no prefix": func(a *store.QueuedAction) { a.IdentityKey = "index:public.x" },
		"bad uuid":  func(a *store.QueuedAction) { a.IdentityKey = ApprovalIdentityPrefix + "nope" },
		"unknown": func(a *store.QueuedAction) {
			a.IdentityKey = ApprovalIdentityPrefix + string(sre.NewUUID())
		},
		"other item id": func(a *store.QueuedAction) { a.ID++ },
	} {
		a := action
		mutate(&a)
		if name != "other item id" && h.actions.Owns(a) {
			t.Errorf("%s: owned", name)
		}
		if _, err := h.actions.RunApproved(h.ctx, a, 3); err == nil {
			t.Errorf("%s: ran", name)
		}
	}
	other := newActionHarness(t, nil)
	if other.actions.Owns(action) {
		t.Fatal("another database's service owns this approval item")
	}
	if h.cancel.callCount() != 0 || other.cancel.callCount() != 0 {
		t.Fatal("a mismatched item reached the executor")
	}
}
