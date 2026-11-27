package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// registerUnpooled registers a fleet member the way startup does when it
// cannot connect: an instance with an error status and no pool.
func registerUnpooled(mgr *fleet.DatabaseManager, name string) {
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: name, Config: config.DatabaseConfig{Name: name},
		Status: &fleet.InstanceStatus{Error: "dial tcp: connection refused"},
	})
}

// Error propagation: a database that was down at startup has no pool. The
// fleet view names it unavailable instead of silently leaving it out.
func TestCasesDatabaseWithoutPoolIsUnavailable(t *testing.T) {
	mgr := seamManager(t, "alpha")
	registerUnpooled(mgr, "zeta")
	registerUnpooled(mgr, "down")
	var mu sync.Mutex
	var readNames []string
	stubDatabaseCases(t, func(_ context.Context, sel namedPool) (databaseCases, error) {
		mu.Lock()
		readNames = append(readNames, sel.name)
		mu.Unlock()
		return databaseCases{cases: []cases.Case{seamCase(sel.name, "a")}}, nil
	})

	code, body := getCases(t, mgr, "")

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got := caseIDs(body.Cases); len(got) != 1 || got[0] != "finding:alpha:a" {
		t.Fatalf("cases = %v, want alpha's one case", got)
	}
	want := []casesUnavailable{
		{Database: "down", ErrorClass: caseErrorConnection},
		{Database: "zeta", ErrorClass: caseErrorConnection},
	}
	if len(body.Unavailable) != 2 || body.Unavailable[0] != want[0] ||
		body.Unavailable[1] != want[1] {
		t.Fatalf("unavailable = %+v, want %+v", body.Unavailable, want)
	}
	if len(readNames) != 1 || readNames[0] != "alpha" {
		t.Fatalf("read %v, want only alpha (no read of a pool-less database)", readNames)
	}
}

// A request naming the pool-less database answers 503 connection, not an
// empty 200; the shadow report does the same.
func TestCasesNamedDatabaseWithoutPoolAnswers503(t *testing.T) {
	mgr := seamManager(t, "alpha")
	registerUnpooled(mgr, "down")
	stubDatabaseCases(t, func(_ context.Context, sel namedPool) (databaseCases, error) {
		return databaseCases{cases: []cases.Case{seamCase(sel.name, "a")}}, nil
	})

	code, body := getCases(t, mgr, "?database=down")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
	if body.ErrorClass != caseErrorConnection || body.Database != "down" {
		t.Fatalf("body = %+v, want database down error_class connection", body)
	}

	code, body = getCases(t, mgr, "?database=alpha")
	if code != http.StatusOK || len(body.Cases) != 1 || len(body.Unavailable) != 0 {
		t.Fatalf("alpha = %d %+v, want 200 with its case and nothing unavailable",
			code, body)
	}
}

// Nil/empty: a fleet whose only member has no pool still answers 200 with
// no cases and that member unavailable.
func TestCasesOnlyDatabaseWithoutPool(t *testing.T) {
	mgr := fleet.NewManager(caseAutonomousConfig())
	registerUnpooled(mgr, "down")
	report := collectCases(context.Background(), mgr, "all")
	if len(report.Cases) != 0 || len(report.Unavailable) != 1 ||
		report.Unavailable[0].Database != "down" {
		t.Fatalf("report = %+v, want no cases and down unavailable", report)
	}
	if !errors.Is(report.firstErr(), errNoDatabasePool) {
		t.Fatalf("first error = %v, want errNoDatabasePool", report.firstErr())
	}
}
