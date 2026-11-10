package value

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// observability-03: /metrics reads only what it exports. IncidentCounts
// counts the ledger by kind in SQL, so it must agree with the detailed
// row scan /api/v1/value keeps using. (Two of the kinds earned-09 kept:
// lock_storm is retired.)
func TestIncidentCountsAgreesWithRowScan(t *testing.T) {
	ctx := context.Background()
	source := newLedgerSource(t, "incident_counts")
	for i, kind := range []string{IncidentXIDWraparound, IncidentDiskFullSlot,
		IncidentXIDWraparound} {
		graph := insertValueEvidenceGraph(t, ctx, source.Pool, 7,
			"counts-"+string(rune('a'+i)))
		if _, err := source.Pool.Exec(ctx, `INSERT INTO sage.incident_avoided
			(kind, severity, credited_minutes, evidence_id, decision_id,
			 action_log_id, verification_id)
			VALUES ($1, 'near_miss', 120, $2, $3, $4, $5)`, kind, graph.evidenceID,
			graph.decisionID, graph.actionID, graph.verificationID); err != nil {
			t.Fatalf("seed %s incident: %v", kind, err)
		}
	}
	counts, err := IncidentCounts(ctx, source.Pool)
	if err != nil {
		t.Fatalf("IncidentCounts: %v", err)
	}
	snapshot, err := readSnapshot(ctx, source.Pool, source.Name, Filter{})
	if err != nil {
		t.Fatalf("readSnapshot: %v", err)
	}
	scanned := map[string]int64{}
	for _, incident := range snapshot.Incidents {
		scanned[incident.Kind]++
	}
	want := map[string]int64{IncidentXIDWraparound: 2, IncidentDiskFullSlot: 1}
	if !reflect.DeepEqual(counts, want) || !reflect.DeepEqual(scanned, want) {
		t.Fatalf("IncidentCounts = %v, row scan = %v, want %v", counts, scanned, want)
	}
}

func TestIncidentCountsEmptyLedgerIsEmptyNotNil(t *testing.T) {
	source := newLedgerSource(t, "incident_counts_empty")
	counts, err := IncidentCounts(context.Background(), source.Pool)
	if err != nil {
		t.Fatalf("IncidentCounts: %v", err)
	}
	if counts == nil || len(counts) != 0 {
		t.Fatalf("empty ledger counts = %#v, want an empty map", counts)
	}
}

// ReadMetrics returns the same per-feature toil as the full snapshot read
// and names the physical database for de-duplication.
func TestReadMetricsMatchesTheSnapshotRead(t *testing.T) {
	ctx := context.Background()
	source := newLedgerSource(t, "read_metrics")
	seedCredit(t, ctx, source.Pool, "analyze_table", 15)
	seedCredit(t, ctx, source.Pool, "analyze_table", 5)
	seedCredit(t, ctx, source.Pool, "create_index", 30)
	got, err := ReadMetrics(ctx, source)
	if err != nil {
		t.Fatalf("ReadMetrics: %v", err)
	}
	results, err := fleetOf(source).Read(ctx, Filter{})
	if err != nil || len(results) != 1 || results[0].Err != nil {
		t.Fatalf("fleet read = %+v, %v", results, err)
	}
	want := map[string]float64{"analyze_table": 20, "create_index": 30}
	if !reflect.DeepEqual(got.ToilMinutesByFeature, want) ||
		!reflect.DeepEqual(results[0].Snapshot.ByFeatureMinutes, want) {
		t.Fatalf("metrics toil = %v, snapshot toil = %v, want %v",
			got.ToilMinutesByFeature, results[0].Snapshot.ByFeatureMinutes, want)
	}
	if got.Identity == "" || !strings.HasSuffix(got.Identity, "/"+source.Pool.Config().
		ConnConfig.Database) {
		t.Fatalf("identity = %q, want <cluster>/<database>", got.Identity)
	}
	if got.IncidentsByKind == nil || len(got.IncidentsByKind) != 0 {
		t.Fatalf("incidents = %#v, want an empty map", got.IncidentsByKind)
	}
}

func TestReadMetricsRejectsUnnamedAndUnconnectedSources(t *testing.T) {
	if _, err := ReadMetrics(context.Background(), Source{}); !errors.Is(err,
		ErrUnnamedSource) {
		t.Fatalf("unnamed source error = %v, want ErrUnnamedSource", err)
	}
	_, err := ReadMetrics(context.Background(), Source{Name: "down"})
	if !errors.Is(err, ErrSourceUnavailable) || !strings.Contains(err.Error(), "down") {
		t.Fatalf("nil pool error = %v, want ErrSourceUnavailable naming the database", err)
	}
}

// A database that never answers fails at the caller's deadline with an
// error naming the database, instead of hanging the caller.
func TestReadMetricsHonoursTheDeadline(t *testing.T) {
	pool := hangingPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	got, err := ReadMetrics(ctx, Source{Name: "blackholed", Pool: pool})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("ReadMetrics took %v past a 200ms deadline", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) ||
		!strings.Contains(err.Error(), "blackholed") {
		t.Fatalf("error = %v, want a deadline error naming the database", err)
	}
	if got.Identity != "" || got.ToilMinutesByFeature != nil || got.IncidentsByKind != nil {
		t.Fatalf("failed read returned data: %+v", got)
	}
}

// The metrics statements carry the pg_sage tag, so pg_stat_statements
// and pg_stat_activity attribute them to the sidecar.
func TestMetricsStatementsAreTagged(t *testing.T) {
	for name, sql := range map[string]string{
		"realized": realizedSQL, "incident counts": incidentCountsSQL,
		"identity": physicalIdentitySQL,
	} {
		if !strings.Contains(sql, "/* pg_sage */") {
			t.Errorf("%s SQL lacks the /* pg_sage */ tag: %q", name, sql)
		}
	}
	if !strings.Contains(incidentCountsSQL, "GROUP BY") {
		t.Fatalf("incident counts must aggregate in SQL: %q", incidentCountsSQL)
	}
}

// hangingPool is a pool whose dialer never connects (a blackholed host).
// The dialer gives up when the test ends so Close never waits on it.
func hangingPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgres://sage@192.0.2.1:5432/hung?sslmode=disable")
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	release := make(chan struct{})
	cfg.ConnConfig.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return nil, errors.New("test finished")
		}
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	t.Cleanup(pool.Close)
	t.Cleanup(func() { close(release) })
	return pool
}
