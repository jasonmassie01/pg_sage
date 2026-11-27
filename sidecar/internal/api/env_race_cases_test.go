package api

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// env-race-b: a per-database cases read that ignores its context is left
// behind when the request answers at its deadline. That goroutine must not
// read the readDatabaseCases seam: ci-19 caught it racing the seam's
// restore in TestCasesSlowDatabaseTimesOutWithinDeadline and
// TestCasesContextHonouringReadTimesOut. Without -race this test only pins
// the answer; under -race the unfixed code reports the race at the
// reassignment below.
func TestRace_Cases_LeftBehindReadDoesNotReadTheSeam(t *testing.T) {
	shortCasesTimeout(t, 50*time.Millisecond)
	mgr := seamManager(t, "wedged")
	release := make(chan struct{})
	var left sync.WaitGroup
	left.Add(1)
	prev := readDatabaseCases
	t.Cleanup(func() { readDatabaseCases = prev })
	readDatabaseCases = func(
		context.Context, *fleet.DatabaseManager, namedPool,
	) (databaseCases, error) {
		defer left.Done()
		<-release // ignores its context, as a wedged driver call would
		return databaseCases{cases: []cases.Case{seamCase("wedged", "late")}}, nil
	}

	code, body := getCases(t, mgr, "?database=wedged")

	// The read is still running. Swapping the seam now is what a test's
	// cleanup (or any later stub) does; nothing orders it with the
	// left-behind goroutine except the handler having read the seam itself.
	readDatabaseCases = prev
	close(release)
	left.Wait()
	if code != http.StatusServiceUnavailable || body.ErrorClass != caseErrorTimeout {
		t.Fatalf("got %d %+v, want 503 timeout for the wedged database", code, body)
	}
	if len(body.Cases) != 0 {
		t.Fatalf("cases = %v, want none: the late result came after the answer",
			caseIDs(body.Cases))
	}
}

// Concurrent access: eight requests in a row, each leaving a wedged read
// behind, so up to eight left-behind reads run while later requests swap
// the seam. Every request answers at its deadline with only the database
// that answered.
func TestRace_Cases_SeamSwapsWhileWedgedReadsRun(t *testing.T) {
	shortCasesTimeout(t, 50*time.Millisecond)
	mgr := seamManager(t, "alpha", "wedged")
	release := make(chan struct{})
	var left sync.WaitGroup
	prev := readDatabaseCases
	t.Cleanup(func() { readDatabaseCases = prev })
	stub := func(_ context.Context, _ *fleet.DatabaseManager, sel namedPool) (
		databaseCases, error,
	) {
		if sel.name != "wedged" {
			return databaseCases{cases: []cases.Case{seamCase("alpha", "a1")}}, nil
		}
		defer left.Done()
		<-release
		return databaseCases{}, nil
	}
	const requests = 8
	left.Add(requests)
	codes := make([]int, requests)
	bodies := make([]casesWire, requests)
	for i := range requests {
		readDatabaseCases = stub // a fresh stub per request, as each test sets one
		codes[i], bodies[i] = getCases(t, mgr, "")
	}
	readDatabaseCases = prev
	close(release)
	left.Wait()
	want := casesUnavailable{Database: "wedged", ErrorClass: caseErrorTimeout}
	for i := range requests {
		if codes[i] != http.StatusOK || len(bodies[i].Unavailable) != 1 ||
			bodies[i].Unavailable[0] != want {
			t.Fatalf("request %d = %d %+v, want 200 with wedged unavailable",
				i, codes[i], bodies[i])
		}
		if got := caseIDs(bodies[i].Cases); len(got) != 1 || got[0] != "finding:alpha:a1" {
			t.Fatalf("request %d cases = %v, want only alpha's", i, got)
		}
	}
}

// Invalid input, nil/empty and boundary do not apply: this is a race fix
// with no new input surface (cases_partial_test.go covers those).
