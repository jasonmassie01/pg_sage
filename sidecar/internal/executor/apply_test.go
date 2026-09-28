package executor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// applyProbe records which stages of an intent ran.
type applyProbe struct {
	admitted, executed, verified atomic.Int32
	deadline                     time.Duration
	executeErr, verifyErr        error
}

func (p *applyProbe) intent() ActionIntent {
	return ActionIntent{
		Request: policy.ActionRequest{SQL: "ANALYZE public.orders"},
		Admit: func(context.Context, int64) error {
			p.admitted.Add(1)
			return nil
		},
		Execute: func(ctx context.Context, _ ActionPolicyDecision) (int64, error) {
			p.executed.Add(1)
			if deadline, ok := ctx.Deadline(); ok {
				p.deadline = time.Until(deadline)
			}
			return 42, p.executeErr
		},
		Verify: func(_ context.Context, actionID int64) error {
			if actionID == 42 {
				p.verified.Add(1)
			}
			return p.verifyErr
		},
	}
}

func applyExecutor(timeoutSeconds int, refuseAt int) (*Executor, *reauthGate) {
	cfg := config.DefaultConfig()
	cfg.Safety.DDLTimeoutSeconds = timeoutSeconds
	e := New(nil, cfg, nil, time.Time{}, nopLog)
	gate := &reauthGate{refuseAt: refuseAt}
	e.WithPolicyGate(gate)
	return e, gate
}

func TestApplyWithoutGateWithholdsEverything(t *testing.T) {
	e := New(nil, config.DefaultConfig(), nil, time.Time{}, nopLog)
	probe := &applyProbe{}
	_, err := e.Apply(context.Background(), probe.intent())
	var withheld *WithheldError
	if !errors.As(err, &withheld) || !errors.Is(err, ErrActionWithheld) ||
		withheld.Reauthorized {
		t.Fatalf("Apply without a gate = %v, want a first-stage WithheldError", err)
	}
	if !strings.Contains(err.Error(), reasonNoStandingPolicy) {
		t.Fatalf("error %q does not give the reason", err)
	}
	if probe.admitted.Load()+probe.executed.Load() != 0 {
		t.Fatal("a refused intent was admitted or executed")
	}
}

func TestApplyFirstRefusalTakesNoSlot(t *testing.T) {
	e, gate := applyExecutor(30, 1)
	probe := &applyProbe{}
	_, err := e.Apply(context.Background(), probe.intent())
	if !errors.Is(err, ErrActionWithheld) || len(gate.remaining) != 1 {
		t.Fatalf("err=%v authorizations=%d, want one refusal", err, len(gate.remaining))
	}
	if probe.admitted.Load() != 0 || len(e.ddlSem) != 0 {
		t.Fatal("refused intent reached admission or held a DDL slot")
	}
}

func TestApplyReauthorizationRefusalStopsExecution(t *testing.T) {
	e, gate := applyExecutor(30, 2)
	probe := &applyProbe{}
	_, err := e.Apply(context.Background(), probe.intent())
	var withheld *WithheldError
	if !errors.As(err, &withheld) || !withheld.Reauthorized {
		t.Fatalf("Apply = %v, want a re-authorization refusal", err)
	}
	if !strings.Contains(err.Error(), "re-authorization refused") {
		t.Fatalf("error %q does not name the re-authorization", err)
	}
	if probe.admitted.Load() != 1 || probe.executed.Load() != 0 || len(gate.remaining) != 2 {
		t.Fatalf("admitted=%d executed=%d authorizations=%d",
			probe.admitted.Load(), probe.executed.Load(), len(gate.remaining))
	}
	if len(e.ddlSem) != 0 {
		t.Fatal("DDL slot leaked after a re-authorization refusal")
	}
}

func TestApplyExecutesUnderMandatoryDeadline(t *testing.T) {
	for _, seconds := range []int{0, -5, 1, 30} {
		e, _ := applyExecutor(seconds, 0)
		probe := &applyProbe{}
		actionID, err := e.Apply(context.Background(), probe.intent())
		if err != nil || actionID != 42 || probe.verified.Load() != 1 {
			t.Fatalf("timeout %ds: id=%d err=%v verified=%d",
				seconds, actionID, err, probe.verified.Load())
		}
		want := time.Duration(seconds) * time.Second
		if seconds <= 0 {
			want = time.Duration(config.DefaultDDLTimeoutSeconds) * time.Second
		}
		if probe.deadline <= want || probe.deadline > want+applyGrace {
			t.Fatalf("timeout %ds: execution deadline %v, want (%v, %v]",
				seconds, probe.deadline, want, want+applyGrace)
		}
		if got := e.ddlTimeout(); got != want {
			t.Fatalf("timeout %ds: statement timeout %v, want %v", seconds, got, want)
		}
	}
}

func TestApplyVerifiesOnlyRecordedSuccess(t *testing.T) {
	e, _ := applyExecutor(30, 0)
	failed := &applyProbe{executeErr: errors.New("lock not available")}
	if _, err := e.Apply(context.Background(), failed.intent()); err == nil ||
		failed.verified.Load() != 0 {
		t.Fatalf("failed execution err=%v verified=%d", err, failed.verified.Load())
	}
	wantErr := errors.New("post-check failed")
	unverified := &applyProbe{verifyErr: wantErr}
	actionID, err := e.Apply(context.Background(), unverified.intent())
	if !errors.Is(err, wantErr) || actionID != 42 {
		t.Fatalf("verify error lost: id=%d err=%v", actionID, err)
	}
}

func TestApplyAuthorizeOnlyNeverExecutes(t *testing.T) {
	e, gate := applyExecutor(30, 0)
	probe := &applyProbe{}
	intent := probe.intent()
	intent.AuthorizeOnly = true
	if _, err := e.Apply(context.Background(), intent); err != nil {
		t.Fatalf("authorize-only intent = %v", err)
	}
	if probe.admitted.Load()+probe.executed.Load() != 0 || len(gate.remaining) != 1 {
		t.Fatal("authorize-only intent ran past its authorization")
	}
}

func TestApplyBusySlotSkipsOrWaits(t *testing.T) {
	e, gate := applyExecutor(30, 0)
	for i := 0; i < cap(e.ddlSem); i++ {
		e.ddlSem <- struct{}{}
	}
	probe := &applyProbe{}
	if _, err := e.Apply(context.Background(), probe.intent()); !errors.Is(err,
		ErrDDLSlotUnavailable) || len(gate.remaining) != 1 {
		t.Fatalf("busy executor err=%v authorizations=%d", err, len(gate.remaining))
	}
	waiting := probe.intent()
	waiting.WaitForSlot = true
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := e.Apply(ctx, waiting); err == nil ||
		!strings.Contains(err.Error(), "DDL slot") {
		t.Fatalf("bounded slot wait = %v, want DDL slot contention", err)
	}
	held := probe.intent()
	held.SlotHeld = true
	if _, err := e.Apply(context.Background(), held); err != nil {
		t.Fatalf("caller-held slot = %v", err)
	}
	if probe.executed.Load() != 1 {
		t.Fatalf("executions = %d, want only the caller-held one", probe.executed.Load())
	}
}

func TestApplyAdmissionFailureStops(t *testing.T) {
	e, gate := applyExecutor(30, 0)
	probe := &applyProbe{}
	intent := probe.intent()
	wantErr := ErrVerificationUnavailable
	intent.Admit = func(context.Context, int64) error { return wantErr }
	if _, err := e.Apply(context.Background(), intent); !errors.Is(err, wantErr) {
		t.Fatalf("admission error = %v", err)
	}
	if probe.executed.Load() != 0 || len(gate.remaining) != 1 || len(e.ddlSem) != 0 {
		t.Fatal("an unadmitted intent was re-authorized, executed or held a slot")
	}
}

func TestApplyNeverExceedsDDLConcurrency(t *testing.T) {
	e, _ := applyExecutor(30, 0)
	var running, peak atomic.Int32
	intent := ActionIntent{
		WaitForSlot: true,
		Execute: func(context.Context, ActionPolicyDecision) (int64, error) {
			now := running.Add(1)
			for {
				seen := peak.Load()
				if now <= seen || peak.CompareAndSwap(seen, now) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			running.Add(-1)
			return 1, nil
		},
	}
	var workers sync.WaitGroup
	for i := 0; i < 12; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if _, err := e.Apply(context.Background(), intent); err != nil {
				t.Errorf("Apply: %v", err)
			}
		}()
	}
	workers.Wait()
	if got := peak.Load(); got < 1 || int(got) > cap(e.ddlSem) {
		t.Fatalf("peak concurrent executions = %d, limit %d", got, cap(e.ddlSem))
	}
}

// A lease held by another writer parks the action (D1): the ledger records
// the park and no failure is reported. Any other lease error is refused and
// recorded as a failure.
func TestApplyParksLeaseConflictAndRecordsOtherLeaseErrors(t *testing.T) {
	pool, ctx := requireDB(t)
	table := probeTable(t, pool, "apply_lease")
	decisionID := recordCustodianDecision(t, ctx, pool, "apply_probe", table)
	objects, err := policy.NormalizeTargetObjects([]string{"public." + table})
	if err != nil {
		t.Fatal(err)
	}
	holder := policy.NewPostgresLeaseManager(pool, nil, decisionID, time.Minute)
	leaseID, err := holder.AcquireLease(ctx, "test", objects, "conflict")
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	t.Cleanup(func() { _ = holder.ReleaseLease(context.Background(), leaseID) })
	e, _ := applyExecutor(30, 0)
	e.pool = pool
	e.WithPolicyGate(&reauthGate{decisionID: decisionID})
	var refused []error
	run := func(target string) (*applyProbe, error) {
		lease := analyzer.Finding{ObjectIdentifier: target,
			RecommendedSQL: "ALTER TABLE public." + table + " SET (fillfactor = 90)"}
		probe := &applyProbe{}
		intent := probe.intent()
		intent.Lease = &lease
		intent.Refused = func(_ context.Context, _ int64, err error) {
			refused = append(refused, err)
		}
		_, err := e.Apply(ctx, intent)
		return probe, err
	}

	probe, err := run("public." + table)
	if !errors.Is(err, policy.ErrLeaseConflict) || probe.executed.Load() != 0 {
		t.Fatalf("conflicting lease = %v, executed=%d", err, probe.executed.Load())
	}
	var parks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.decision
		WHERE verdict='parked' AND (evidence->>'parked_decision_id')::bigint=$1`,
		decisionID).Scan(&parks); err != nil {
		t.Fatal(err)
	}
	if parks != 1 || len(refused) != 0 || len(e.ddlSem) != 0 {
		t.Fatalf("parks=%d refused=%v, want one park and no failure", parks, refused)
	}

	probe, err = run("unqualified_target")
	if err == nil || probe.executed.Load() != 0 || len(refused) != 1 {
		t.Fatalf("invalid lease target err=%v refused=%v", err, refused)
	}
}
