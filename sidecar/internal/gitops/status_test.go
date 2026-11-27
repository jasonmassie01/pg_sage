package gitops

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// The Settings banner reads Status: the last reconcile's diff, or its
// error with every problem.

func TestStatus_RecordsTheLastRunAndItsError(t *testing.T) {
	var s Status
	if got := s.Report(); got.At != (time.Time{}) || got.Error != "" {
		t.Fatalf("zero status = %+v, want nothing recorded", got)
	}
	at := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	res := Result{Mode: ModeApply, Hash: "abc", Applied: true,
		Changes: []Change{{Kind: KindPrincipal, Name: "ci-bot", Op: OpCreate}}}
	s.Record("/etc/sage/policy", res, nil, at)
	got := s.Report()
	if got.Directory != "/etc/sage/policy" || got.Hash != "abc" || !got.Applied ||
		len(got.Changes) != 1 || got.Error != "" || !got.At.Equal(at) {
		t.Fatalf("report = %+v", got)
	}
	invalid := &InvalidError{Problems: []Problem{{File: "grants/g.yaml", Line: 4,
		Message: "names database ledger, which pg_sage does not monitor"}}}
	s.Record("/etc/sage/policy", Result{Mode: ModeApply}, fmt.Errorf("wrap: %w", invalid),
		at.Add(time.Minute))
	got = s.Report()
	if got.Error == "" || len(got.Problems) != 1 || got.Problems[0].Line != 4 ||
		got.Applied || len(got.Changes) != 0 {
		t.Fatalf("failed report = %+v, want the error and its problem", got)
	}
	s.Record("/etc/sage/policy", Result{}, ErrDirectoryMissing, at.Add(2*time.Minute))
	if got := s.Report(); !got.Missing || got.Error == "" {
		t.Fatalf("missing directory report = %+v", got)
	}
	s.Record("/etc/sage/policy", Result{}, errors.New("connection refused"), at)
	if got := s.Report(); got.Missing || got.Error != "connection refused" {
		t.Fatalf("database error report = %+v", got)
	}
}

func TestStatus_ConcurrentRecordAndRead(t *testing.T) {
	var s Status
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			s.Record("d", Result{Hash: fmt.Sprint(i)}, nil, time.Now())
		}(i)
		go func() {
			defer wg.Done()
			_ = s.Report()
		}()
	}
	wg.Wait()
	if s.Report().Hash == "" {
		t.Fatal("no run recorded")
	}
}

func TestResult_WritesCountsOnlyWrites(t *testing.T) {
	r := Result{Changes: []Change{{Op: OpCreate}, {Op: OpHold}, {Op: OpWaiting},
		{Op: OpRatify}, {Op: OpPropose}}}
	if r.Writes() != 3 {
		t.Fatalf("writes = %d, want create, ratify and propose", r.Writes())
	}
	if (Result{}).Writes() != 0 {
		t.Fatal("an empty result has writes")
	}
}
