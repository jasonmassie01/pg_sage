package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// cases-11: open incident counts feed the Overview hero. A count that
// could not be read is unknown (-1), never 0, so the hero cannot claim
// health on a failed read.
// No concurrent access tests here: these helpers hold no shared state;
// InstanceStatus locking is covered by the fleet package.

// fakeIncidentRows yields (severity, count) pairs, then rowsErr.
type fakeIncidentRows struct {
	sev     []string
	cnt     []int
	i       int
	rowsErr error
}

func (r *fakeIncidentRows) Close()                        {}
func (r *fakeIncidentRows) Err() error                    { return r.rowsErr }
func (r *fakeIncidentRows) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }
func (r *fakeIncidentRows) FieldDescriptions() []pgconn.FieldDescription {
	return nil
}
func (r *fakeIncidentRows) Values() ([]any, error) { return nil, nil }
func (r *fakeIncidentRows) RawValues() [][]byte    { return nil }
func (r *fakeIncidentRows) Conn() *pgx.Conn        { return nil }

func (r *fakeIncidentRows) Next() bool {
	if r.i >= len(r.sev) {
		return false
	}
	r.i++
	return true
}

func (r *fakeIncidentRows) Scan(dest ...any) error {
	*dest[0].(*string) = r.sev[r.i-1]
	*dest[1].(*int) = r.cnt[r.i-1]
	return nil
}

// fakeIncidentQuerier returns rows or err and records the query and
// whether its context carried a deadline.
type fakeIncidentQuerier struct {
	rows        *fakeIncidentRows
	err         error
	sql         string
	hadDeadline bool
}

func (q *fakeIncidentQuerier) Query(
	ctx context.Context, sql string, _ ...any,
) (pgx.Rows, error) {
	q.sql = sql
	deadline, ok := ctx.Deadline()
	q.hadDeadline = ok && time.Until(deadline) <= 5*time.Second
	if q.err != nil {
		return nil, q.err
	}
	return q.rows, nil
}

func TestReadIncidentCounts_CountsOpenBySeverity(t *testing.T) {
	q := &fakeIncidentQuerier{rows: &fakeIncidentRows{
		sev: []string{"critical", "warning", "info"}, cnt: []int{2, 3, 7}}}
	got, err := readIncidentCounts(context.Background(), q)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.critical != 2 || got.warning != 3 || got.info != 7 || got.open != 12 {
		t.Fatalf("counts %+v, want critical 2 warning 3 info 7 open 12", got)
	}
	if !strings.Contains(q.sql, "sage.incidents") ||
		!strings.Contains(q.sql, "resolved_at IS NULL") {
		t.Fatalf("query %q does not read unresolved incidents", q.sql)
	}
	if !q.hadDeadline {
		t.Fatal("incident query ran without a 5s deadline")
	}
}

func TestReadIncidentCounts_QueryErrorPropagates(t *testing.T) {
	q := &fakeIncidentQuerier{err: errors.New(`relation "sage.incidents" does not exist`)}
	_, err := readIncidentCounts(context.Background(), q)
	if err == nil || !strings.Contains(err.Error(), "incidents") ||
		!strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err %v, want the incidents query error with its cause", err)
	}
}

func TestReadIncidentCounts_RowsErrorPropagates(t *testing.T) {
	q := &fakeIncidentQuerier{rows: &fakeIncidentRows{sev: []string{"critical"},
		cnt: []int{1}, rowsErr: errors.New("connection reset mid-read")}}
	_, err := readIncidentCounts(context.Background(), q)
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("err %v, want the mid-read error", err)
	}
}

func TestApplyIncidentCounts_ErrorMarksUnknown(t *testing.T) {
	s := &fleet.InstanceStatus{IncidentsCritical: 4, IncidentsWarning: 2}
	applyIncidentCounts(s, severityCounts{critical: 9}, errors.New("timeout"))
	if s.IncidentsCritical != fleet.IncidentsUnknown ||
		s.IncidentsWarning != fleet.IncidentsUnknown {
		t.Fatalf("after error: critical %d warning %d, want -1 and -1",
			s.IncidentsCritical, s.IncidentsWarning)
	}
}

func TestApplyIncidentCounts_KnownCountsReplaceUnknown(t *testing.T) {
	s := &fleet.InstanceStatus{IncidentsCritical: fleet.IncidentsUnknown,
		IncidentsWarning: fleet.IncidentsUnknown}
	applyIncidentCounts(s, severityCounts{critical: 1, warning: 0, info: 5}, nil)
	if s.IncidentsCritical != 1 || s.IncidentsWarning != 0 {
		t.Fatalf("critical %d warning %d, want 1 and 0",
			s.IncidentsCritical, s.IncidentsWarning)
	}
}

func TestApplyIncidentCounts_EmptyResultIsZero(t *testing.T) {
	q := &fakeIncidentQuerier{rows: &fakeIncidentRows{}}
	got, err := readIncidentCounts(context.Background(), q)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := &fleet.InstanceStatus{IncidentsCritical: fleet.IncidentsUnknown}
	applyIncidentCounts(s, got, nil)
	if s.IncidentsCritical != 0 || s.IncidentsWarning != 0 {
		t.Fatalf("no incidents read as critical %d warning %d",
			s.IncidentsCritical, s.IncidentsWarning)
	}
}

// A database that failed to connect or build never read its incidents:
// its placeholder row reports them unknown, and the summary counts it
// under incidents_unknown instead of as zero open incidents.
func TestFailedPlaceholders_IncidentsUnknown(t *testing.T) {
	placeholders := map[string]*fleet.DatabaseInstance{
		"config": failedFleetInstance(config.DatabaseConfig{Name: "cfg"},
			errors.New("connection refused")),
		"store": failedStoreInstance(store.DatabaseRecord{ID: 7, Name: "st"},
			"build failed"),
	}
	for kind, inst := range placeholders {
		s := inst.SnapshotStatus()
		if s.IncidentsCritical != fleet.IncidentsUnknown ||
			s.IncidentsWarning != fleet.IncidentsUnknown {
			t.Errorf("%s placeholder incidents critical %d warning %d, want -1",
				kind, s.IncidentsCritical, s.IncidentsWarning)
		}
		if s.Error == "" || s.Connected {
			t.Errorf("%s placeholder lost its failure: %+v", kind, s)
		}
	}
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(placeholders["config"])
	mgr.RegisterInstance(placeholders["store"])
	sum := mgr.FleetStatus().Summary
	if sum.IncidentsUnknown != 2 || sum.IncidentsCritical != 0 || sum.Degraded != 2 {
		t.Fatalf("summary incidents_unknown %d critical %d degraded %d, want 2, 0, 2",
			sum.IncidentsUnknown, sum.IncidentsCritical, sum.Degraded)
	}
}
