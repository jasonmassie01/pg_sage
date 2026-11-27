package cases

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/backendguard"
	"github.com/pg-sage/sidecar/internal/signalid"
)

var (
	blockerBackendStart = time.Date(2026, 11, 6, 9, 15, 30, 123456000, time.UTC)
	blockerQueryStart   = time.Date(2026, 11, 6, 9, 40, 1, 654321000, time.UTC)
)

func activeBlocker() IncidentBlocker {
	return IncidentBlocker{
		PID: 4242, BackendStart: blockerBackendStart, QueryStart: blockerQueryStart,
		QuerySHA: "deadbeefcafef00d", State: "active",
		TotalBlocked: 3, LockedRelation: "public.orders",
	}
}

// lockIncident mirrors rca.lockContentionIncident: a summary link plus one
// link per root blocker carrying its identity.
func lockIncident(severity Severity, blockers ...IncidentBlocker) SourceIncident {
	chain := []IncidentChainLink{{
		Order: 1, Signal: signalid.LockContention,
		Description: "Lock chain contention detected",
		Evidence:    fmt.Sprintf("%d lock chains, 3 total blocked", len(blockers)),
	}}
	for k := range blockers {
		b := blockers[k]
		chain = append(chain, IncidentChainLink{
			Order: k + 2, Signal: signalid.LockContention,
			Description: fmt.Sprintf("Root blocker pid %d (%s) blocks 3 sessions",
				b.PID, b.State),
			Evidence: fmt.Sprintf("pid=%d query_sha=%s", b.PID, b.QuerySHA),
			Blocker:  &b,
		})
	}
	return SourceIncident{
		ID: "inc-lock", DatabaseName: "prod", Severity: severity,
		RootCause:   "Lock chain: root blocker pid 4242 (active) blocks 3 sessions",
		CausalChain: chain, SignalIDs: []string{signalid.LockContention},
		ActionRisk: "safe", Source: "deterministic", Confidence: 0.85,
		LastDetectedAt: blockerQueryStart.Add(time.Minute),
	}
}

func candidateOf(t *testing.T, c Case, actionType string) ActionCandidate {
	t.Helper()
	for _, candidate := range c.ActionCandidates {
		if candidate.ActionType == actionType {
			return candidate
		}
	}
	t.Fatalf("no %s candidate in %+v", actionType, c.ActionCandidates)
	return ActionCandidate{}
}

func hasCandidate(c Case, actionType string) bool {
	for _, candidate := range c.ActionCandidates {
		if candidate.ActionType == actionType {
			return true
		}
	}
	return false
}

func assertGuardedScript(t *testing.T, c ActionCandidate, fragments ...string) {
	t.Helper()
	if c.BlockedReason != "" {
		t.Fatalf("%s is blocked: %q", c.ActionType, c.BlockedReason)
	}
	if !reflect.DeepEqual(c.OutputModes, []string{"script"}) {
		t.Fatalf("%s OutputModes = %v, want script only", c.ActionType, c.OutputModes)
	}
	base := []string{
		"a.pid = 4242",
		"a.backend_start = '2026-11-06T09:15:30.123456Z'::timestamptz",
		"a.query_start = '2026-11-06T09:40:01.654321Z'::timestamptz",
		"left(encode(sha256(convert_to(left(a.query, 200), 'UTF8')), 'hex'), 16)" +
			" = 'deadbeefcafef00d'",
		"a.backend_type = 'client backend'",
		"a.datname = current_database()",
		backendguard.ProtectedPredicate,
	}
	for _, fragment := range append(base, fragments...) {
		if !strings.Contains(c.ProposedSQL, fragment) {
			t.Fatalf("%s script lacks %q:\n%s", c.ActionType, fragment, c.ProposedSQL)
		}
	}
	if barePIDSignal.MatchString(c.ProposedSQL) {
		t.Fatalf("%s script signals a bare PID:\n%s", c.ActionType, c.ProposedSQL)
	}
	if c.ExpiresAt == nil || c.RollbackClass != "not_reversible" {
		t.Fatalf("%s expiry/rollback = %v/%q", c.ActionType, c.ExpiresAt, c.RollbackClass)
	}
}

// Happy path: one active root blocker gives a guarded cancel script pinned
// to pid + backend_start + query_start + query hash; a warning incident
// offers no terminate.
func TestActiveLockBlockerGivesGuardedCancelScript(t *testing.T) {
	got := ProjectIncident(lockIncident(SeverityWarning, activeBlocker()))

	if len(got.ActionCandidates) != 2 {
		t.Fatalf("ActionCandidates = %+v, want diagnostic + cancel", got.ActionCandidates)
	}
	assertCandidate(t, got.ActionCandidates[0], "diagnose_lock_blockers", "safe", "")
	cancel := candidateOf(t, got, "cancel_backend")
	if cancel.RiskTier != "moderate" {
		t.Fatalf("cancel RiskTier = %q, want moderate", cancel.RiskTier)
	}
	assertGuardedScript(t, cancel, "SELECT pg_cancel_backend(a.pid)", "a.state = 'active'")
	if strings.Contains(cancel.ProposedSQL, "pg_terminate_backend") {
		t.Fatalf("cancel script terminates:\n%s", cancel.ProposedSQL)
	}
	want := activeBlocker()
	if cancel.BackendIdentity == nil || !reflect.DeepEqual(*cancel.BackendIdentity, want) {
		t.Fatalf("BackendIdentity = %+v, want %+v", cancel.BackendIdentity, want)
	}
	if hasCandidate(got, "terminate_backend") {
		t.Fatal("warning lock incident offered terminate")
	}
}

// Boundary: severity critical is the only thing that adds terminate for an
// active blocker, and that terminate is HIGH and refuses superusers.
func TestCriticalActiveLockBlockerAddsGuardedTerminate(t *testing.T) {
	got := ProjectIncident(lockIncident(SeverityCritical, activeBlocker()))

	if len(got.ActionCandidates) != 3 {
		t.Fatalf("ActionCandidates = %+v, want diagnostic + cancel + terminate",
			got.ActionCandidates)
	}
	terminate := candidateOf(t, got, "terminate_backend")
	if terminate.RiskTier != "high" {
		t.Fatalf("terminate RiskTier = %q, want high", terminate.RiskTier)
	}
	assertGuardedScript(t, terminate, "SELECT pg_terminate_backend(a.pid)",
		"a.state = 'active'", backendguard.TerminateJoin, backendguard.TerminatePredicate)
	assertGuardedScript(t, candidateOf(t, got, "cancel_backend"),
		"SELECT pg_cancel_backend(a.pid)")
}

// Nil/empty/invalid identity: any missing or malformed part of the
// identity blocks the signal, even on a critical incident.
func TestLockBlockerMissingIdentityIsBlocked(t *testing.T) {
	cases := map[string]func(*IncidentBlocker){
		"zero pid":            func(b *IncidentBlocker) { b.PID = 0 },
		"negative pid":        func(b *IncidentBlocker) { b.PID = -7 },
		"zero backend_start":  func(b *IncidentBlocker) { b.BackendStart = time.Time{} },
		"zero query_start":    func(b *IncidentBlocker) { b.QueryStart = time.Time{} },
		"empty query_sha":     func(b *IncidentBlocker) { b.QuerySHA = "" },
		"short query_sha":     func(b *IncidentBlocker) { b.QuerySHA = "deadbeef" },
		"uppercase query_sha": func(b *IncidentBlocker) { b.QuerySHA = "DEADBEEFCAFEF00D" },
		"injected query_sha":  func(b *IncidentBlocker) { b.QuerySHA = "dead'; DROP x;--" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := activeBlocker()
			mutate(&b)

			got := ProjectIncident(lockIncident(SeverityCritical, b))

			assertBlockedSignal(t, candidateOf(t, got, "cancel_backend"),
				"no single structured blocker identity")
			if hasCandidate(got, "terminate_backend") {
				t.Fatal("incomplete identity offered terminate")
			}
			assertNoBarePIDSignal(t, got)
		})
	}
}

// Boundary: two root blockers mean no single identity; cancelling one
// would not clear the chain, so neither is offered.
func TestTwoRootBlockersGiveNoSignalCandidate(t *testing.T) {
	second := activeBlocker()
	second.PID = 4343
	second.QuerySHA = "0123456789abcdef"

	got := ProjectIncident(lockIncident(SeverityCritical, activeBlocker(), second))

	if len(got.ActionCandidates) != 2 {
		t.Fatalf("ActionCandidates = %+v, want diagnostic + blocked cancel",
			got.ActionCandidates)
	}
	assertBlockedSignal(t, got.ActionCandidates[1], "no single structured blocker identity")
	if got.ActionCandidates[1].BackendIdentity != nil {
		t.Fatalf("blocked cancel names an identity: %+v",
			got.ActionCandidates[1].BackendIdentity)
	}
}

// State transition: an idle-in-transaction blocker cannot be cancelled
// (pg_cancel_backend does nothing to an idle session), so cancel is blocked
// and terminate is offered pinned to the idle state and query_start, at any
// severity.
func TestIdleInTxBlockerGivesBlockedCancelAndGuardedTerminate(t *testing.T) {
	b := activeBlocker()
	b.State = "idle in transaction"

	got := ProjectIncident(lockIncident(SeverityWarning, b))

	if len(got.ActionCandidates) != 3 {
		t.Fatalf("ActionCandidates = %+v, want diagnostic + cancel + terminate",
			got.ActionCandidates)
	}
	cancel := candidateOf(t, got, "cancel_backend")
	assertBlockedSignal(t, cancel, "idle in transaction")
	terminate := candidateOf(t, got, "terminate_backend")
	if terminate.RiskTier != "high" {
		t.Fatalf("terminate RiskTier = %q, want high", terminate.RiskTier)
	}
	assertGuardedScript(t, terminate, "SELECT pg_terminate_backend(a.pid)",
		"a.state = 'idle in transaction'", backendguard.TerminatePredicate)
	if strings.Contains(terminate.ProposedSQL, "a.state = 'active'") {
		t.Fatalf("idle terminate pins the active state:\n%s", terminate.ProposedSQL)
	}
}

// State transition: a state with no guarded signal (idle, aborted, empty)
// blocks the signal rather than guessing.
func TestUnsupportedBlockerStateIsBlocked(t *testing.T) {
	for _, state := range []string{"", "idle", "idle in transaction (aborted)", "fastpath"} {
		b := activeBlocker()
		b.State = state

		got := ProjectIncident(lockIncident(SeverityCritical, b))

		assertBlockedSignal(t, candidateOf(t, got, "cancel_backend"), "state")
		if hasCandidate(got, "terminate_backend") {
			t.Fatalf("state %q offered terminate", state)
		}
	}
}

// Rollout: incident rows written before playbooks-01 have no blocker on
// their links. Their prose PID must not come back as a signal.
func TestLockLinkWithoutBlockerIsBlocked(t *testing.T) {
	incident := lockIncident(SeverityCritical, activeBlocker())
	for k := range incident.CausalChain {
		incident.CausalChain[k].Blocker = nil
	}

	got := ProjectIncident(incident)

	assertBlockedSignal(t, candidateOf(t, got, "cancel_backend"),
		"no single structured blocker identity")
	if hasCandidate(got, "terminate_backend") {
		t.Fatal("blocker-less lock incident offered terminate")
	}
	assertNoBarePIDSignal(t, got)
}

// Only lock_contention links carry a signal-grade identity; a blocker on
// any other link is ignored.
func TestBlockerOnNonLockLinkIsIgnored(t *testing.T) {
	incident := lockIncident(SeverityCritical, activeBlocker())
	for k := range incident.CausalChain {
		incident.CausalChain[k].Signal = signalid.VacuumBlocked
	}

	got := ProjectIncident(incident)

	assertBlockedSignal(t, candidateOf(t, got, "cancel_backend"),
		"no single structured blocker identity")
}

// Timestamps render as UTC RFC3339Nano literals whatever zone they carry.
func TestGuardedScriptRendersTimesInUTC(t *testing.T) {
	b := activeBlocker()
	zone := time.FixedZone("CST", -6*3600)
	b.BackendStart = b.BackendStart.In(zone)
	b.QueryStart = b.QueryStart.In(zone)

	got := ProjectIncident(lockIncident(SeverityWarning, b))

	assertGuardedScript(t, candidateOf(t, got, "cancel_backend"))
}

// Concurrent access: projection is pure; concurrent projections of one
// incident (sharing its blocker pointers) agree and do not mutate it.
func TestConcurrentLockProjectionIsStable(t *testing.T) {
	incident := lockIncident(SeverityCritical, activeBlocker())
	want := ProjectIncident(incident)
	var wg sync.WaitGroup
	results := make([]Case, 20)
	for k := range results {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			results[k] = ProjectIncident(incident)
		}(k)
	}
	wg.Wait()
	for k, got := range results {
		if !reflect.DeepEqual(got.ActionCandidates, want.ActionCandidates) {
			t.Fatalf("projection %d differs", k)
		}
	}
	if !reflect.DeepEqual(*incident.CausalChain[1].Blocker, activeBlocker()) {
		t.Fatalf("projection mutated the blocker: %+v", incident.CausalChain[1].Blocker)
	}
}

// CHECK-02 over every incident shape the projector handles.
func TestNoIncidentCandidateUsesBarePIDSignal(t *testing.T) {
	idle := activeBlocker()
	idle.State = "idle in transaction"
	runaway := SourceIncident{ID: "r", DatabaseName: "prod", Severity: SeverityCritical,
		RootCause: "Query PID 222 ran for 45 minutes", SignalIDs: []string{signalid.LogSlowQuery}}
	for _, incident := range []SourceIncident{
		testIncident("prod"), idleTxSaturationIncident(SeverityCritical), runaway,
		lockIncident(SeverityCritical, activeBlocker()),
		lockIncident(SeverityCritical, idle),
		lockIncident(SeverityCritical),
	} {
		assertNoBarePIDSignal(t, ProjectIncident(incident))
	}
}
