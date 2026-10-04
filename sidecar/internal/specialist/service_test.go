package specialist

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
)

// The specialist service: open/attach, status, result and remediation
// requests, with scopes, per-database permission, rate limits and bounded
// concurrency enforced before any backend call.

var (
	reader = Identity{TokenID: "tok-read", Name: "datadog", Kind: "agent",
		Scopes: []string{"read"}, Databases: []string{"orders"}, Transport: "http"}
	proposer = Identity{TokenID: "tok-prop", Name: "aws devops agent", Kind: "agent",
		Scopes: []string{"read", "propose"}, Databases: []string{"orders"}, Transport: "http"}
)

type harness struct {
	svc    *Service
	orders *fakeBackend
	other  *fakeBackend
	store  *memStore
	clock  *fakeClock
}

func newHarness(t *testing.T, limits Limits) *harness {
	t.Helper()
	h := &harness{orders: newFakeBackend(), other: newFakeBackend(), store: newMemStore(),
		clock: &fakeClock{now: created.Add(5 * time.Minute)}}
	svc, err := NewService(Deps{Directory: fakeDirectory{"orders": h.orders,
		"billing": h.other}, Store: h.store, Limits: limits, KeepIdentifiers: true,
		Now: h.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	h.svc = svc
	return h
}

func symptomReq(family string) OpenRequest {
	return OpenRequest{Symptom: &Symptom{Summary: "checkout p99 4s"}, Family: family}
}

func TestNewService_RequiresItsDependencies(t *testing.T) {
	if _, err := NewService(Deps{Store: newMemStore(), Limits: DefaultLimits()}); !errors.Is(
		err, ErrInvalid) {
		t.Fatalf("no directory: %v", err)
	}
	_, err := NewService(Deps{Directory: fakeDirectory{}, Limits: DefaultLimits()})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("no store: %v", err)
	}
	if _, err := NewService(Deps{Directory: fakeDirectory{}, Store: newMemStore(),
		Limits: Limits{}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero limits: %v", err)
	}
}

func TestOpen_StartsAnInvestigationAsTheNamedAgent(t *testing.T) {
	h := newHarness(t, DefaultLimits())
	resp, err := h.svc.Open(context.Background(), reader, "orders", symptomReq("lock_blocking"))
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Created || resp.Match != "new" || resp.ContractVersion != ContractVersion ||
		resp.Database != "orders" || resp.Investigation.State != "queued" ||
		resp.Investigation.Terminal || !strings.HasSuffix(resp.Links.Result,
		"/databases/orders/investigations/"+resp.Investigation.ID+"/result") {
		t.Fatalf("response %+v", resp)
	}
	if len(h.orders.started) != 1 {
		t.Fatalf("started %+v", h.orders.started)
	}
	tr := h.orders.started[0]
	if tr.Kind != sre.TriggerLock || tr.Actor != reader.Actor() ||
		!strings.HasPrefix(tr.CaseID, "specialist:") || strings.Contains(tr.Subject, "checkout") {
		t.Fatalf("trigger %+v: the caller's text must not become the subject", tr)
	}
	recs := h.store.all()
	if len(recs) != 1 || recs[0].Kind != KindOpen || !recs[0].Created ||
		recs[0].TokenID != "tok-read" || recs[0].IdentityName != "datadog" ||
		recs[0].Actor != reader.Actor() || recs[0].InvestigationID != resp.Investigation.ID ||
		recs[0].Symptom == nil || recs[0].Outbound != OutboundNone {
		t.Fatalf("audit record %+v", recs)
	}
}

func TestOpen_WithoutFamilyOpensTheOperatorTriage(t *testing.T) {
	h := newHarness(t, DefaultLimits())
	if _, err := h.svc.Open(context.Background(), reader, "orders", symptomReq("")); err != nil {
		t.Fatal(err)
	}
	if h.orders.started[0].Kind != sre.TriggerOperator {
		t.Fatalf("kind %q", h.orders.started[0].Kind)
	}
}

func TestOpen_RepeatCoalescesIntoTheLiveInvestigation(t *testing.T) {
	h := newHarness(t, DefaultLimits())
	ctx := context.Background()
	first, err := h.svc.Open(ctx, reader, "orders", symptomReq("lock_blocking"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.svc.Open(ctx, proposer, "orders", symptomReq("lock_blocking"))
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || second.Match != "idempotent" ||
		second.Investigation.ID != first.Investigation.ID {
		t.Fatalf("second open %+v", second)
	}
}

func TestOpen_IdempotencyKeyIsScopedToTheIdentity(t *testing.T) {
	h := newHarness(t, DefaultLimits())
	ctx := context.Background()
	req := symptomReq("lock_blocking")
	req.IdempotencyKey = "pd:Q1"
	if _, err := h.svc.Open(ctx, reader, "orders", req); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Open(ctx, proposer, "orders", req); err != nil {
		t.Fatal(err)
	}
	a, b := h.orders.started[0].IdempotencyKey, h.orders.started[1].IdempotencyKey
	if a == "" || a == b || len(a) > 160 || strings.Contains(a, "pd:Q1") {
		t.Fatalf("keys %q %q: hashed per identity, bounded", a, b)
	}
}

func TestOpen_AttachByInvestigationID(t *testing.T) {
	h := newHarness(t, DefaultLimits())
	h.orders.put(lockDetail())
	resp, err := h.svc.Open(context.Background(), reader, "orders",
		OpenRequest{Attach: &Attach{InvestigationID: string(inv)}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Created || resp.Match != "investigation_id" || resp.Investigation.ID != string(inv) ||
		!resp.Investigation.Terminal || len(h.orders.started) != 0 {
		t.Fatalf("attach %+v", resp)
	}
	if recs := h.store.all(); len(recs) != 1 || recs[0].Kind != KindAttach || recs[0].Created {
		t.Fatalf("attach record %+v", recs)
	}
	// An id of another database is not found under this one.
	h.other.put(lockDetail())
	_, err = h.svc.Open(context.Background(), reader, "orders", OpenRequest{
		Attach: &Attach{InvestigationID: "77777777-7777-4777-8777-777777777777"}})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestOpen_AttachByIncidentID(t *testing.T) {
	h := newHarness(t, DefaultLimits())
	d := lockDetail()
	d.Investigation.IncidentID = "rca-41"
	h.orders.put(d)
	resp, err := h.svc.Open(context.Background(), reader, "orders",
		OpenRequest{Attach: &Attach{IncidentID: "rca-41"}})
	if err != nil || resp.Match != "incident_id" || resp.Investigation.ID != string(inv) {
		t.Fatalf("incident attach %+v %v", resp, err)
	}
	_, err = h.svc.Open(context.Background(), reader, "orders",
		OpenRequest{Attach: &Attach{IncidentID: "rca-404"}})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown incident: %v", err)
	}
}

func TestOpen_WindowAttachesToTheSameFamilyInTheWindow(t *testing.T) {
	h := newHarness(t, DefaultLimits())
	h.orders.put(lockDetail()) // lock_blocking, created 11:50
	req := symptomReq("lock_blocking")
	req.Window = &Window{Start: created.Add(-10 * time.Minute)}
	resp, err := h.svc.Open(context.Background(), reader, "orders", req)
	if err != nil || resp.Match != "window" || resp.Investigation.ID != string(inv) {
		t.Fatalf("window attach %+v %v", resp, err)
	}
	// Another family in the window is not attached.
	req.Family = "wal_retention"
	resp, err = h.svc.Open(context.Background(), reader, "orders", req)
	if err != nil || resp.Match != "new" || !resp.Created {
		t.Fatalf("other family opens new %+v %v", resp, err)
	}
	// A window that ended before the investigation is not attached.
	end := created.Add(-30 * time.Minute)
	req.Family, req.Window = "lock_blocking", &Window{Start: created.Add(-time.Hour), End: &end}
	h.orders.setState(inv, sre.StateConcluded)
	resp, err = h.svc.Open(context.Background(), reader, "orders", req)
	if err != nil || resp.Match == "window" {
		t.Fatalf("window before the investigation %+v %v", resp, err)
	}
}

func TestOpen_RefusalsHappenBeforeAnyBackendCall(t *testing.T) {
	h := newHarness(t, DefaultLimits())
	ctx := context.Background()
	noScope := Identity{TokenID: "t", Name: "x", Kind: "agent", Scopes: []string{"propose"}}
	if _, err := h.svc.Open(ctx, noScope, "orders", symptomReq("")); !errors.Is(err,
		ErrScope) {
		t.Fatalf("no read scope: %v", err)
	}
	// Not permitted: the same refusal for an existing and a missing database.
	for _, db := range []string{"billing", "does_not_exist"} {
		if _, err := h.svc.Open(ctx, reader, db, symptomReq("")); !errors.Is(err,
			ErrDatabaseNotPermitted) {
			t.Fatalf("%s: %v", db, err)
		}
	}
	all := Identity{TokenID: "t2", Name: "all", Kind: "agent", Scopes: []string{"read"}}
	if _, err := h.svc.Open(ctx, all, "does_not_exist", symptomReq("")); !errors.Is(err,
		ErrNotFound) {
		t.Fatalf("unknown database for an unrestricted token: %v", err)
	}
	if len(h.orders.started)+len(h.other.started) != 0 || len(h.store.all()) != 0 {
		t.Fatal("a refused call reached a backend or the store")
	}
}

func TestOpen_RateLimitedPerIdentity(t *testing.T) {
	limits := DefaultLimits()
	limits.WritesPerMinute = 2
	h := newHarness(t, limits)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := h.svc.Open(ctx, reader, "orders", symptomReq("lock_blocking")); err != nil {
			t.Fatal(err)
		}
	}
	_, err := h.svc.Open(ctx, reader, "orders", symptomReq("lock_blocking"))
	var rl *RateLimitError
	if !errors.Is(err, ErrRateLimited) || !errors.As(err, &rl) || rl.RetryAfter <= 0 {
		t.Fatalf("third write: %v", err)
	}
	if _, err := h.svc.Open(ctx, proposer, "orders", symptomReq("lock_blocking")); err != nil {
		t.Fatalf("another identity is not limited: %v", err)
	}
	// Reads have their own, larger budget.
	if _, err := h.svc.Status(ctx, reader, "orders", h.store.all()[0].InvestigationID); err != nil {
		t.Fatalf("reads are not charged to the write budget: %v", err)
	}
}

func TestOpen_BoundedConcurrentInvestigations(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxOpenPerIdentity, limits.MaxOpenTotal = 2, 2
	h := newHarness(t, limits)
	ctx := context.Background()
	families := []string{"lock_blocking", "wal_retention", "plan_regression"}
	for _, f := range families[:2] {
		if _, err := h.svc.Open(ctx, reader, "orders", symptomReq(f)); err != nil {
			t.Fatal(err)
		}
	}
	_, err := h.svc.Open(ctx, reader, "orders", symptomReq(families[2]))
	if !errors.Is(err, ErrTooManyInvestigations) {
		t.Fatalf("third live investigation of one identity: %v", err)
	}
	// Attaching never counts against the bound.
	first := h.store.all()[0].InvestigationID
	if _, err := h.svc.Open(ctx, reader, "orders", OpenRequest{Attach: &Attach{
		InvestigationID: first}}); err != nil {
		t.Fatalf("attach at the bound: %v", err)
	}
	// One concludes: the slot frees.
	h.orders.setState(sre.UUID(first), sre.StateConcluded)
	if _, err := h.svc.Open(ctx, reader, "orders", symptomReq(families[2])); err != nil {
		t.Fatalf("after one concluded: %v", err)
	}
	// The total bound across identities.
	_, err = h.svc.Open(ctx, proposer, "orders", symptomReq("replication_lag"))
	if !errors.Is(err, ErrTooManyInvestigations) {
		t.Fatalf("total bound: %v", err)
	}
}

func TestOpen_ConcurrentOpensRespectTheBound(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxOpenPerIdentity, limits.MaxOpenTotal, limits.WritesPerMinute = 2, 10, 100
	h := newHarness(t, limits)
	families := []string{"lock_blocking", "wal_retention", "plan_regression",
		"replication_lag", "checkpoint_storm", "lwlock_contention"}
	var wg sync.WaitGroup
	var mu sync.Mutex
	opened := 0
	for _, f := range families {
		wg.Add(1)
		go func(f string) {
			defer wg.Done()
			resp, err := h.svc.Open(context.Background(), reader, "orders", symptomReq(f))
			if err == nil && resp.Created {
				mu.Lock()
				opened++
				mu.Unlock()
			}
		}(f)
	}
	wg.Wait()
	if opened != 2 {
		t.Fatalf("%d investigations opened in parallel over a bound of 2", opened)
	}
}

func TestOpen_ErrorsPropagateDistinguishably(t *testing.T) {
	h := newHarness(t, DefaultLimits())
	ctx := context.Background()
	h.orders.startErr = sre.ErrMetadataUnavailable
	if _, err := h.svc.Open(ctx, reader, "orders", symptomReq("lock_blocking")); !errors.Is(err,
		ErrUnavailable) {
		t.Fatalf("store outage: %v", err)
	}
	h.orders.startErr = nil
	h.store.err = errors.New("connection refused")
	_, err := h.svc.Open(ctx, reader, "orders", symptomReq("lock_blocking"))
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("audit store failure: %v", err)
	}
}

func TestStatus_PhasesAndPolling(t *testing.T) {
	h := newHarness(t, DefaultLimits())
	ctx := context.Background()
	d := lockDetail()
	cases := map[sre.State]string{sre.StateQueued: "queued",
		sre.StateCollecting: "collecting_evidence", sre.StateNeedsEvidence: "collecting_evidence",
		sre.StateEvaluating: "evaluating", sre.StatePaused: "paused",
		sre.StateConcluded: "complete", sre.StateFailed: "complete"}
	for st, phase := range cases {
		d.Investigation.State = st
		h.orders.put(d)
		s, err := h.svc.Status(ctx, reader, "orders", string(inv))
		if err != nil {
			t.Fatal(err)
		}
		wantPoll := 5
		if st.Terminal() {
			wantPoll = 0
		}
		if s.Phase != phase || s.PollAfterSeconds != wantPoll || s.ProbeCount != 4 ||
			s.ContractVersion != ContractVersion || s.Investigation.State != string(st) {
			t.Fatalf("%s: %+v", st, s)
		}
	}
	if _, err := h.svc.Status(ctx, reader, "orders", "not-a-uuid"); !errors.Is(err,
		ErrInvalid) {
		t.Fatalf("bad id: %v", err)
	}
	if _, err := h.svc.Status(ctx, reader, "billing", string(inv)); !errors.Is(err,
		ErrDatabaseNotPermitted) {
		t.Fatalf("other database: %v", err)
	}
}

func TestResult_IncludesProposalsCalibrationAndCallerRecord(t *testing.T) {
	cal := &CalibratedRate{Family: "lock_blocking", K: 20, N: 20, Rate: 1, WilsonLower: 0.84,
		Source: "bench"}
	h := newHarness(t, DefaultLimits())
	h.svc.calibrator = calibratorFunc(func(_ context.Context, db, family string) (
		*CalibratedRate, error) {
		if db != "orders" || family != "lock_blocking" {
			return nil, nil
		}
		return cal, nil
	})
	h.orders.put(lockDetail())
	h.orders.proposals[inv] = []sreaction.ProposalView{cancelProposal(
		sreaction.ProposalProposed)}
	ctx := context.Background()
	if _, err := h.svc.Open(ctx, proposer, "orders", OpenRequest{Symptom: &Symptom{
		Summary: "slow"}, Attach: &Attach{InvestigationID: string(inv)}}); err != nil {
		t.Fatal(err)
	}
	r, err := h.svc.Result(ctx, proposer, "orders", string(inv))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Remediations) != 1 || r.Confidence.Calibration != "bench_top1" ||
		r.CallerSupplied == nil || r.CallerSupplied.Summary != "slow" {
		t.Fatalf("result %+v", r)
	}
	// Another identity does not see this caller's text.
	r, err = h.svc.Result(ctx, reader, "orders", string(inv))
	if err != nil || r.CallerSupplied != nil {
		t.Fatalf("caller text leaked across identities: %+v %v", r.CallerSupplied, err)
	}
}

func TestResult_FailuresAreNeverDisguised(t *testing.T) {
	h := newHarness(t, DefaultLimits())
	h.orders.put(lockDetail())
	ctx := context.Background()
	// A calibration failure leaves the result uncalibrated, never invented.
	h.svc.calibrator = calibratorFunc(func(context.Context, string, string) (*CalibratedRate,
		error) {
		return nil, errors.New("ledger unreadable")
	})
	r, err := h.svc.Result(ctx, reader, "orders", string(inv))
	if err != nil || r.Confidence.Calibration != "uncalibrated" {
		t.Fatalf("calibration failure: %+v %v", r.Confidence, err)
	}
	// A database without an action service has no cancel proposals.
	h.orders.propErr = ErrNoActions
	if r, err = h.svc.Result(ctx, reader, "orders", string(inv)); err != nil ||
		len(r.Remediations) != 0 {
		t.Fatalf("no action service: %+v %v", r.Remediations, err)
	}
	h.orders.propErr = sre.ErrMetadataUnavailable
	if _, err = h.svc.Result(ctx, reader, "orders", string(inv)); !errors.Is(err,
		ErrUnavailable) {
		t.Fatalf("proposal store outage must not look like no proposals: %v", err)
	}
}
