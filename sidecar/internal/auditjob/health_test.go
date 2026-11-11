package auditjob

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// audit-12: the registry keeps the last verification result of every
// (database, chain), so /metrics and GET /api/v1/audit/status can report
// audit health without verifying on demand.

var healthT0 = time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC)

func cleanReport(chain string, head int64, at time.Time) auditchain.Report {
	return auditchain.Report{Chain: chain, Mode: "full", HeadSeq: head, VerifiedAt: at}
}

func brokenReport(chain string, at time.Time, kinds ...string) auditchain.Report {
	rep := cleanReport(chain, 9, at)
	for i, k := range kinds {
		rep.Problems = append(rep.Problems, auditchain.Problem{Kind: k, Seq: int64(i + 1)})
	}
	return rep
}

func chainOf(t *testing.T, s HealthSnapshot, database, chain string) ChainHealth {
	t.Helper()
	for _, c := range s.Chains {
		if c.Database == database && c.Chain == chain {
			return c
		}
	}
	t.Fatalf("no health for %s/%s in %+v", database, chain, s.Chains)
	return ChainHealth{}
}

func errorsOf(s HealthSnapshot, database string) int64 {
	for _, e := range s.Errors {
		if e.Database == database {
			return e.Count
		}
	}
	return 0
}

// Happy path: a clean chain is ok with no problems; a broken one counts
// its problems by kind; the snapshot is sorted by database then chain.
func TestHealthRecordsEachChain(t *testing.T) {
	h := NewHealthRegistry()
	h.Record("orders", []auditchain.Report{
		cleanReport("auth_audit", 4, healthT0),
		brokenReport("action_log", healthT0, auditchain.KindRowEdited,
			auditchain.KindRowEdited, auditchain.KindGap),
	}, nil, healthT0.Add(time.Second))
	h.Record("billing", []auditchain.Report{cleanReport("action_log", 2, healthT0)}, nil,
		healthT0)
	s := h.Snapshot()
	if len(s.Chains) != 3 {
		t.Fatalf("chains = %+v, want 3", s.Chains)
	}
	order := []string{"billing/action_log", "orders/action_log", "orders/auth_audit"}
	for i, c := range s.Chains {
		if got := c.Database + "/" + c.Chain; got != order[i] {
			t.Fatalf("chain %d = %s, want %s (sorted)", i, got, order[i])
		}
	}
	bad := chainOf(t, s, "orders", "action_log")
	if bad.OK || bad.Problems[auditchain.KindRowEdited] != 2 ||
		bad.Problems[auditchain.KindGap] != 1 || len(bad.Problems) != 2 {
		t.Fatalf("broken chain = %+v", bad)
	}
	good := chainOf(t, s, "orders", "auth_audit")
	if !good.OK || len(good.Problems) != 0 || good.HeadSeq != 4 ||
		!good.VerifiedAt.Equal(healthT0) || good.Mode != "full" {
		t.Fatalf("clean chain = %+v", good)
	}
	if s.VerifiedAt == nil || !s.VerifiedAt.Equal(healthT0) {
		t.Fatalf("snapshot verified_at = %v, want %v", s.VerifiedAt, healthT0)
	}
	if errorsOf(s, "orders") != 0 || len(s.Errors) != 0 {
		t.Fatalf("clean runs counted errors: %+v", s.Errors)
	}
}

// Nil/empty: nothing recorded is an empty snapshot whose verified_at is
// nil, not the zero time; a nil registry behaves the same.
func TestHealthEmptyAndNil(t *testing.T) {
	for name, h := range map[string]*HealthRegistry{"new": NewHealthRegistry(), "nil": nil} {
		s := h.Snapshot()
		if len(s.Chains) != 0 || len(s.Errors) != 0 || s.VerifiedAt != nil {
			t.Fatalf("%s registry snapshot = %+v, want empty with nil verified_at", name, s)
		}
		h.Record("orders", nil, nil, healthT0)
		h.Retain(nil)
		h.Reset()
	}
	h := NewHealthRegistry()
	h.Record("orders", nil, nil, healthT0)
	if s := h.Snapshot(); len(s.Chains) != 0 || s.VerifiedAt != nil {
		t.Fatalf("a run without chains recorded %+v", s)
	}
}

// Boundary: a report without its own time takes the run's; a truncated
// problem list is not ok even with no problem listed.
func TestHealthTimeAndTruncation(t *testing.T) {
	h := NewHealthRegistry()
	rep := cleanReport("action_log", 1, time.Time{})
	trunc := cleanReport("auth_audit", 1, healthT0)
	trunc.ProblemsTruncated = true
	h.Record("orders", []auditchain.Report{rep, trunc}, nil, healthT0.Add(time.Minute))
	s := h.Snapshot()
	if c := chainOf(t, s, "orders", "action_log"); !c.VerifiedAt.Equal(
		healthT0.Add(time.Minute)) {
		t.Fatalf("report without a time verified_at = %v, want the run's", c.VerifiedAt)
	}
	if c := chainOf(t, s, "orders", "auth_audit"); c.OK || !c.ProblemsTruncated {
		t.Fatalf("truncated report = %+v, want not ok", c)
	}
	if !s.VerifiedAt.Equal(healthT0.Add(time.Minute)) {
		t.Fatalf("snapshot verified_at = %v, want the newest chain's", s.VerifiedAt)
	}
}

// Error propagation and state transitions: a run that errors keeps the
// previous result of every chain it did not report and counts the error;
// a clean run replaces the database's chains, dropping one no longer
// installed; a broken chain that verifies again turns ok.
func TestHealthErrorKeepsPreviousResult(t *testing.T) {
	h := NewHealthRegistry()
	h.Record("orders", []auditchain.Report{cleanReport("action_log", 3, healthT0),
		cleanReport("auth_audit", 1, healthT0)}, nil, healthT0)
	h.Record("orders", nil, errors.New("auditjob: orders: closed pool"),
		healthT0.Add(time.Hour))
	s := h.Snapshot()
	if c := chainOf(t, s, "orders", "action_log"); !c.OK || !c.VerifiedAt.Equal(healthT0) {
		t.Fatalf("after a failed run action_log = %+v, want the previous result", c)
	}
	if n := errorsOf(s, "orders"); n != 1 {
		t.Fatalf("verify errors = %d, want 1", n)
	}
	if s.Errors[0].LastError != "auditjob: orders: closed pool" ||
		!s.Errors[0].LastAt.Equal(healthT0.Add(time.Hour)) {
		t.Fatalf("error entry = %+v", s.Errors[0])
	}
	// A partial failure: one chain reported broken, the other errored.
	h.Record("orders", []auditchain.Report{brokenReport("action_log", healthT0.Add(2*time.Hour),
		auditchain.KindRowEdited)}, errors.New("auth_audit: timeout"), healthT0.Add(2*time.Hour))
	s = h.Snapshot()
	if c := chainOf(t, s, "orders", "action_log"); c.OK {
		t.Fatalf("broken report not recorded on a partial failure: %+v", c)
	}
	if c := chainOf(t, s, "orders", "auth_audit"); !c.OK || !c.VerifiedAt.Equal(healthT0) {
		t.Fatalf("errored chain lost its previous result: %+v", c)
	}
	if n := errorsOf(s, "orders"); n != 2 {
		t.Fatalf("verify errors = %d, want 2", n)
	}
	h.Record("orders", []auditchain.Report{cleanReport("action_log", 5,
		healthT0.Add(3*time.Hour))}, nil, healthT0.Add(3*time.Hour))
	s = h.Snapshot()
	if c := chainOf(t, s, "orders", "action_log"); !c.OK || c.HeadSeq != 5 {
		t.Fatalf("re-verified chain = %+v, want ok at 5", c)
	}
	if len(s.Chains) != 1 {
		t.Fatalf("a clean run kept a chain it did not report: %+v", s.Chains)
	}
	if n := errorsOf(s, "orders"); n != 2 {
		t.Fatalf("a clean run reset the error counter to %d (it is a counter)", n)
	}
}

// State transitions: Retain drops the databases no longer verified here
// (removed from the fleet); Reset drops everything (this sidecar is no
// longer the leader, so it must not export a stale result).
func TestHealthRetainAndReset(t *testing.T) {
	h := NewHealthRegistry()
	for _, db := range []string{"orders", "gone", "billing"} {
		h.Record(db, []auditchain.Report{cleanReport("action_log", 1, healthT0)},
			errors.New("x"), healthT0)
	}
	h.Retain([]string{"orders", "billing"})
	s := h.Snapshot()
	for _, c := range s.Chains {
		if c.Database == "gone" {
			t.Fatalf("Retain kept a removed database: %+v", s.Chains)
		}
	}
	if len(s.Chains) != 2 || errorsOf(s, "gone") != 0 || len(s.Errors) != 2 {
		t.Fatalf("after Retain: chains %+v errors %+v", s.Chains, s.Errors)
	}
	h.Reset()
	if s := h.Snapshot(); len(s.Chains) != 0 || len(s.Errors) != 0 || s.VerifiedAt != nil {
		t.Fatalf("after Reset: %+v", s)
	}
}

// The snapshot is a copy: changing its problem map never changes the
// registry.
func TestHealthSnapshotIsACopy(t *testing.T) {
	h := NewHealthRegistry()
	h.Record("orders", []auditchain.Report{brokenReport("action_log", healthT0,
		auditchain.KindGap)}, nil, healthT0)
	s := h.Snapshot()
	s.Chains[0].Problems[auditchain.KindGap] = 99
	if got := h.Snapshot().Chains[0].Problems[auditchain.KindGap]; got != 1 {
		t.Fatalf("snapshot aliases the registry: gap = %d", got)
	}
}

// Concurrent access: verification runs record while scrapes read.
func TestHealthConcurrentRecordAndSnapshot(t *testing.T) {
	h := NewHealthRegistry()
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(2)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				db := fmt.Sprintf("db%d", w)
				h.Record(db, []auditchain.Report{cleanReport("action_log", int64(i),
					healthT0)}, errors.New("e"), healthT0)
			}
		}(w)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_ = h.Snapshot()
			}
		}()
	}
	wg.Wait()
	s := h.Snapshot()
	if len(s.Chains) != 4 || len(s.Errors) != 4 {
		t.Fatalf("after concurrent runs: %d chains, %d error rows", len(s.Chains), len(s.Errors))
	}
	for _, e := range s.Errors {
		if e.Count != 200 {
			t.Fatalf("%s counted %d errors, want 200 (lost updates)", e.Database, e.Count)
		}
	}
}

func TestDefaultHealthIsOneRegistry(t *testing.T) {
	if DefaultHealth() == nil || DefaultHealth() != DefaultHealth() {
		t.Fatal("DefaultHealth must return one process-wide registry")
	}
}
