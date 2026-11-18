package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/migration"
	"github.com/pg-sage/sidecar/internal/review/trigger"
	"github.com/pg-sage/sidecar/internal/upgradecheck"
)

// review-04a: the upgrade readiness and migration lint routes publish a
// review trigger. A failed publish is logged and counted (the trigger
// package counts it); it never fails the route's own answer.

type hookRecorder struct {
	mu       sync.Mutex
	calls    int
	database string
	major    int
	tables   []trigger.TableSize
	err      error
}

func (h *hookRecorder) upgrade(_ context.Context, database string,
	t upgradecheck.Target) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	h.database, h.major = database, t.Major
	return h.err
}

func (h *hookRecorder) lint(_ context.Context, database string,
	tables []trigger.TableSize) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	h.database, h.tables = database, tables
	return h.err
}

func TestUpgradeReadiness_PublishesPreUpgradeAndSurvivesAFailedPublish(t *testing.T) {
	for _, hookErr := range []error{nil, errors.New("insert into sage.review_trigger: down")} {
		f := &fakeUpgrade{rep: upgradecheck.Report{SourceMajor: 16}}
		rec := &hookRecorder{err: hookErr}
		d := upgradeDeps(f)
		d.trigger = rec.upgrade
		code, body := getUpgrade(t, d, "database=prod&target_major=18")
		if code != http.StatusOK {
			t.Fatalf("hook err %v: status %d %s, want 200", hookErr, code, body)
		}
		if rec.calls != 1 || rec.database != "prod" || rec.major != 18 {
			t.Fatalf("hook %+v, want one pre_upgrade for prod at 18", rec)
		}
	}
}

func TestUpgradeReadiness_FailedCheckPublishesNothing(t *testing.T) {
	f := &fakeUpgrade{err: errors.New("catalog read timed out")}
	rec := &hookRecorder{}
	d := upgradeDeps(f)
	d.trigger = rec.upgrade
	if code, _ := getUpgrade(t, d, "target_major=18"); code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", code)
	}
	if rec.calls != 0 {
		t.Fatalf("a failed check published %d triggers", rec.calls)
	}
}

func TestMigrationLint_PublishesTheMeasuredTablesAndSurvivesAFailedPublish(t *testing.T) {
	for _, hookErr := range []error{nil, errors.New("no review queue")} {
		rec := &hookRecorder{err: hookErr}
		d := lintDeps(&lintCatalog{rows: 30_000_000}, 30)
		d.trigger = rec.lint
		resp := postLint(t, migrationLintHandler(d), testViewerUser(),
			`{"sql":"CREATE INDEX x ON app.t (c)"}`)
		if resp.Code != http.StatusOK {
			t.Fatalf("hook err %v: status %d %s", hookErr, resp.Code, resp.Body)
		}
		if rec.calls != 1 || len(rec.tables) != 1 || rec.tables[0].Table != "app.t" ||
			rec.tables[0].Bytes != 3_000_000_000 {
			t.Fatalf("hook %+v, want app.t at 3 GB", rec)
		}
	}
	// A static review measures nothing, so it publishes nothing.
	rec := &hookRecorder{}
	d := lintDeps(&lintCatalog{rows: 30_000_000}, 30)
	d.trigger = rec.lint
	resp := postLint(t, migrationLintHandler(d), testViewerUser(),
		`{"sql":"CREATE INDEX x ON app.t (c)","static":true}`)
	if resp.Code != http.StatusOK || rec.calls != 0 {
		t.Fatalf("static: %d, %d publishes, want none", resp.Code, rec.calls)
	}
}

func TestLintTables_KeepsEachMeasuredTableOnceAtItsLargestSize(t *testing.T) {
	rep := migration.Report{Statements: []migration.StatementReview{
		{Hazards: []migration.Hazard{{Table: "app.t", SizeKnown: true, TableSizeBytes: 5},
			{Table: "app.u", SizeKnown: false, TableSizeBytes: 0}}},
		{Hazards: []migration.Hazard{{Table: "app.t", SizeKnown: true, TableSizeBytes: 9},
			{Table: "", SizeKnown: true, TableSizeBytes: 7}}},
	}}
	got := lintTables(rep)
	if len(got) != 1 || got[0].Table != "app.t" || got[0].Bytes != 9 {
		t.Fatalf("lintTables = %+v, want app.t at 9 bytes", got)
	}
	if got := lintTables(migration.Report{}); len(got) != 0 {
		t.Fatalf("empty report = %+v", got)
	}
}
