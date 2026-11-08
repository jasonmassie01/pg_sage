package main

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/autoexplain"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/lakebaseobs"
	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

type historyWiringReader struct{ calls int }

func (r *historyWiringReader) ReadWindow(context.Context, lakebaseobs.Stream, lakebaseobs.Scope, lakebaseobs.Window) (lakebaseobs.Batch, error) {
	r.calls++
	return lakebaseobs.Batch{}, errors.New("synthetic SDK outage")
}

type historyWiringFixture struct {
	ctx       context.Context
	control   *pgxpool.Pool
	monitored *pgxpool.Pool
	store     *lakebaseobs.Store
	now       time.Time
}

func newHistoryWiringFixture(t *testing.T) *historyWiringFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	control, err := pgxpool.New(ctx, testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(control.Close)
	monitored, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "history_monitored"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(monitored.Close)
	for _, p := range []*pgxpool.Pool{control, monitored} {
		if err := schema.Bootstrap(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	store, err := lakebaseobs.NewStore(control)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	return &historyWiringFixture{ctx: ctx, control: control, monitored: monitored, store: store, now: time.Now().UTC().Truncate(time.Microsecond)}
}

func (f *historyWiringFixture) registration(t *testing.T) (*databaseRuntime, lakebaseobs.Scope, config.LakebaseObservabilityConfig) {
	t.Helper()
	name := "history-wiring-" + uuid.NewString()
	st, err := sre.NewPostgresStore(f.control, sre.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: st,
		Runner: probes.NewRunner(f.monitored, probes.Catalog(), nil), Config: sre.DefaultCoordinatorConfig(name)})
	if err != nil {
		t.Fatal(err)
	}
	registered, err := coord.Bind(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	conn := f.monitored.Config().ConnConfig
	binding := config.LakebaseObservabilityBinding{Host: conn.Host, Database: conn.Database,
		Endpoint: "projects/owned-test/branches/owned-test/endpoints/primary"}
	settings := config.LakebaseObservabilityConfig{Enabled: true, WarehouseID: "synthetic-warehouse",
		AllowPayload: true, Bindings: []config.LakebaseObservabilityBinding{binding}}
	var oid int64
	if err := f.monitored.QueryRow(f.ctx, "SELECT oid::bigint FROM pg_database WHERE datname=current_database()").Scan(&oid); err != nil {
		t.Fatal(err)
	}
	scope := lakebaseobs.Scope{DeploymentID: string(registered.DeploymentID), DatabaseID: string(registered.DatabaseID),
		Workspace: "https://synthetic.databricks.com", ProjectID: "owned-project", BranchID: "owned-branch", EndpointID: "owned-endpoint",
		DatabaseName: conn.Database, DatabaseOID: oid, PolicyBinding: lakebasePolicyBinding(binding)}
	rt := &databaseRuntime{provider: "lakebase", sre: coord, cfg: config.DefaultConfig(),
		spec: databaseRuntimeSpec{Name: name, Pool: f.monitored, ControlPool: f.control}}
	rt.cfg.LakebaseObservability = settings
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		scopes, _ := f.store.OwnerScopes(cleanup, scope.DeploymentID, scope.DatabaseID)
		for _, hash := range scopes {
			_, _ = f.control.Exec(cleanup, "DELETE FROM sage.lakebaseobs_inbox WHERE binding LIKE $1", hash+":%")
			_, _ = f.control.Exec(cleanup, "DELETE FROM sage.lakebaseobs_cursors WHERE binding LIKE $1", hash+":%")
		}
		_, _ = f.control.Exec(cleanup, "DELETE FROM sage.lakebaseobs_owners WHERE deployment_id=$1 AND database_id=$2", scope.DeploymentID, scope.DatabaseID)
	})
	return rt, scope, settings
}

func historyWiringEvent(t *testing.T, scope lakebaseobs.Scope, stream lakebaseobs.Stream, at, received time.Time, overrides map[string]string) lakebaseobs.Event {
	t.Helper()
	fields := map[string]string{"project_id": scope.ProjectID, "branch_id": scope.BranchID, "endpoint_id": scope.EndpointID,
		"compute_id": "owned-compute-epoch", "ts": at.Format(time.RFC3339Nano), "dbid": strconv.FormatInt(scope.DatabaseOID, 10),
		"datid":   strconv.FormatInt(scope.DatabaseOID, 10),
		"datname": scope.DatabaseName, "application_name": "owned_fixture_app", "queryid": "18446744073709551615",
		"query_text": "SELECT 42", "plan_json": `{"Plan":{"Node Type":"Result","Total Cost":1.25}}`,
		"truncated": "false", "duration_ms": "12.5", "analyze_enabled": "false", "buffers_enabled": "true", "plan_fingerprint": "owned-fingerprint",
		"backend_pid": "91", "severity": "ERROR", "sqlstate": "40P01", "message": "synthetic deadlock", "truncated_mask": "0",
		"pid": "91", "blocking_pid": "92", "wait_event_type": "Lock", "wait_event": "transactionid", "xact_age_ms": "1200",
		"command_tag": "CREATE TABLE", "object_type": "table", "object_name": "owned_fixture", "schema_name": "owned_fixture",
		"previous_ts": at.Add(-time.Minute).Format(time.RFC3339Nano), "xact_commit": "10", "host_memory_total_bytes": "1048576"}
	for key, value := range overrides {
		fields[key] = value
	}
	e, err := lakebaseobs.NewEvent(stream, scope, fields, received, true)
	if err != nil {
		t.Fatalf("synthetic stream %s event: %v", stream, err)
	}
	return e
}

func (f *historyWiringFixture) runtime(t *testing.T, scope lakebaseobs.Scope) (*lakebaseobs.Runtime, *historyWiringReader) {
	t.Helper()
	r := &historyWiringReader{}
	runtime, err := lakebaseobs.NewRuntime(r, f.store, scope, lakebaseobs.Options{WarehouseID: "synthetic-warehouse", AllowPayload: true,
		Streams: []lakebaseobs.Stream{lakebaseobs.Plans, lakebaseobs.Logs, lakebaseobs.ASH, lakebaseobs.DDL, lakebaseobs.DatabaseCounters}})
	if err != nil {
		t.Fatal(err)
	}
	return runtime, r
}

func (f *historyWiringFixture) insert(t *testing.T, scope lakebaseobs.Scope, runtime *lakebaseobs.Runtime, events ...lakebaseobs.Event) {
	t.Helper()
	if err := f.store.BindOwner(f.ctx, scope); err != nil {
		t.Fatal(err)
	}
	groups := map[lakebaseobs.Stream][]lakebaseobs.Event{}
	for _, e := range events {
		groups[e.Stream] = append(groups[e.Stream], e)
	}
	for stream, group := range groups {
		before, err := f.store.LoadCursor(f.ctx, runtime.Binding(), stream)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.store.CommitBatch(f.ctx, runtime.Binding(), stream, before, before, group, lakebaseobs.SourceRetention); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *historyWiringFixture) insertPlan(t *testing.T, scope lakebaseobs.Scope, at time.Time) (*lakebaseobs.Runtime, lakebaseobs.Event) {
	t.Helper()
	runtime, _ := f.runtime(t, scope)
	e := historyWiringEvent(t, scope, lakebaseobs.Plans, at, f.now, nil)
	f.insert(t, scope, runtime, e)
	p, err := lakebaseobs.NormalizePlan(e)
	if err != nil {
		t.Fatal(err)
	}
	if err := autoexplain.StoreProviderPlan(f.ctx, f.monitored, autoexplain.ProviderPlan{
		ObservedPlan: autoexplain.ObservedPlan{QueryID: p.QueryID, Query: p.Query, JSON: p.JSON, TotalCost: p.TotalCost, ExecutionMS: p.ExecutionMS, CapturedAt: p.CapturedAt},
		ScopeHash:    scope.Hash(), ComputeID: e.ComputeID, EventID: e.ID, ReceivedAt: f.now, AnalyzeEnabled: p.AnalyzeEnabled,
		BuffersEnabled: p.BuffersEnabled, TimingEnabled: p.TimingEnabled, Fingerprint: p.Fingerprint, DurationSource: p.DurationSource}); err != nil {
		t.Fatal(err)
	}
	return runtime, e
}

func registerHistoryWiring(t *testing.T, name string, st *lakebaseTelemetryState) {
	t.Helper()
	lakebaseTelemetryRegistry.Lock()
	previous, existed := lakebaseTelemetryRegistry.entries[name]
	lakebaseTelemetryRegistry.entries[name] = st
	lakebaseTelemetryRegistry.Unlock()
	t.Cleanup(func() {
		lakebaseTelemetryRegistry.Lock()
		defer lakebaseTelemetryRegistry.Unlock()
		if existed {
			lakebaseTelemetryRegistry.entries[name] = previous
		} else {
			delete(lakebaseTelemetryRegistry.entries, name)
		}
	})
}

func TestLakebaseTelemetryHistoryNormalizationAndRegistryIsolation(t *testing.T) {
	f := newHistoryWiringFixture(t)
	rt, scope, _ := f.registration(t)
	_, other, _ := f.registration(t)
	runtime, _ := f.runtime(t, scope)
	foreign, _ := f.runtime(t, other)
	stamp := f.now.Add(-2 * time.Minute)
	for _, s := range []lakebaseobs.Stream{lakebaseobs.Plans, lakebaseobs.Logs, lakebaseobs.ASH, lakebaseobs.DDL, lakebaseobs.DatabaseCounters} {
		f.insert(t, scope, runtime, historyWiringEvent(t, scope, s, stamp, f.now, nil))
		f.insert(t, other, foreign, historyWiringEvent(t, other, s, stamp.Add(time.Second), f.now, map[string]string{"message": "other-registration-marker"}))
	}
	f.insert(t, scope, runtime,
		historyWiringEvent(t, scope, lakebaseobs.Plans, stamp.Add(2*time.Second), f.now, map[string]string{"plan_json": "<REDACTED>"}),
		historyWiringEvent(t, scope, lakebaseobs.Plans, stamp.Add(3*time.Second), f.now, map[string]string{"truncated": "true"}),
		historyWiringEvent(t, scope, lakebaseobs.Plans, stamp.Add(4*time.Second), f.now, map[string]string{"application_name": "pg_sage"}))
	registerHistoryWiring(t, rt.spec.Name, &lakebaseTelemetryState{runtime: runtime, store: f.store})
	foreignName := rt.spec.Name + ":other"
	registerHistoryWiring(t, foreignName, &lakebaseTelemetryState{runtime: foreign, store: f.store})
	for _, s := range []lakebaseobs.Stream{lakebaseobs.Plans, lakebaseobs.Logs, lakebaseobs.ASH, lakebaseobs.DDL, lakebaseobs.DatabaseCounters} {
		result, err := lakebaseTelemetryHistory(f.ctx, rt.spec.Name, string(s), f.now.Add(-time.Hour), f.now, 20)
		if err != nil {
			t.Fatal(err)
		}
		view := result.(map[string]any)
		rows := view["rows"].([]map[string]any)
		want := 1
		if s == lakebaseobs.Plans {
			want = 4
		}
		if view["historical_only"] != true || view["complete"] != false || view["exact_multiplicity"] != false || len(rows) != want {
			t.Fatalf("history invented coverage or crossed registry: stream=%s view=%+v", s, view)
		}
		seen := map[string]bool{}
		for _, row := range rows {
			if row["scope_hash"] != scope.Hash() || row["compute_id"] != "owned-compute-epoch" || row["historical_only"] != true || !row["received_at"].(time.Time).Equal(f.now) {
				t.Fatalf("history lost resource/time provenance: %+v", row)
			}
			availability := row["availability"].(string)
			seen[availability] = true
			if availability != "observed" {
				if _, exists := row["observation"]; exists {
					t.Fatalf("unavailable payload exposed as observation: %+v", row)
				}
				continue
			}
			switch s {
			case lakebaseobs.Plans:
				p := row["observation"].(lakebaseobs.Plan)
				if p.QueryID != -1 || p.AnalyzeEnabled == nil || *p.AnalyzeEnabled || p.BuffersEnabled == nil || !*p.BuffersEnabled || p.TimingEnabled != nil || p.DurationSource != "lakebase_plan_history.duration_ms" || p.Fingerprint != "owned-fingerprint" || !p.CapturedAt.Equal(stamp) {
					t.Fatalf("plan capture provenance flattened: %+v", p)
				}
			case lakebaseobs.Logs:
				l := row["observation"].(logwatch.LogEntry)
				if l.SQLState != "40P01" || l.Message != "synthetic deadlock" || l.SessionID != "" || !l.Timestamp.Equal(stamp) {
					t.Fatalf("log occurrence/session semantics invented: %+v", l)
				}
			case lakebaseobs.ASH:
				sample := row["observation"].(lakebaseobs.SessionSample)
				if sample.PID != 91 || sample.BlockingPID != 92 || sample.QueryID != -1 || !sample.At.Equal(stamp) {
					t.Fatalf("ASH identity changed: %+v", sample)
				}
			case lakebaseobs.DDL:
				ddl := row["observation"].(lakebaseobs.DDLChange)
				if ddl.Command != "CREATE TABLE" || ddl.ObjectName != "owned_fixture" || !ddl.At.Equal(stamp) {
					t.Fatalf("DDL provenance changed: %+v", ddl)
				}
			case lakebaseobs.DatabaseCounters:
				metric := row["observation"].(lakebaseobs.MetricInterval)
				if !metric.HistoricalOnly || !metric.Counters || metric.Values["xact_commit"] != 10 || !metric.To.Equal(stamp) || !metric.From.Equal(stamp.Add(-time.Minute)) {
					t.Fatalf("historical interval became a live/cumulative value: %+v", metric)
				}
			}
		}
		if !seen["observed"] || (s == lakebaseobs.Plans && (!seen["payload_unavailable"] || !seen["rejected_row"] || !seen["self_excluded"])) {
			t.Fatalf("history availability distinctions lost: %+v", seen)
		}
	}
	result, err := lakebaseTelemetryHistory(f.ctx, foreignName, string(lakebaseobs.Logs), f.now.Add(-time.Hour), f.now, 20)
	if err != nil || len(result.(map[string]any)["rows"].([]map[string]any)) != 1 {
		t.Fatalf("other registration unavailable: %v", err)
	}
	for _, state := range []*lakebaseTelemetryState{nil, {}, {store: f.store}, {runtime: runtime}} {
		name := "unavailable-history-" + uuid.NewString()
		if state != nil {
			registerHistoryWiring(t, name, state)
		}
		if _, err := lakebaseTelemetryHistory(f.ctx, name, string(lakebaseobs.Plans), f.now.Add(-time.Hour), f.now, 10); err == nil {
			t.Fatal("unavailable registration borrowed another database's history")
		}
	}
}

func (f *historyWiringFixture) assertPayloadCopies(t *testing.T, scope lakebaseobs.Scope, runtime *lakebaseobs.Runtime, want int) {
	t.Helper()
	for name, query := range map[string]struct {
		pool *pgxpool.Pool
		sql  string
		key  string
	}{
		"control inbox":   {f.control, "SELECT count(*) FROM sage.lakebaseobs_inbox WHERE binding=$1 AND event IS NOT NULL", runtime.Binding()},
		"monitored cache": {f.monitored, "SELECT count(*) FROM sage.explain_cache WHERE source='lakebase_plan_history' AND provider_scope_hash=$1", scope.Hash()},
	} {
		var n int
		if err := query.pool.QueryRow(f.ctx, query.sql, query.key).Scan(&n); err != nil || n != want {
			t.Fatalf("%s payload copies=%d want=%d err=%v", name, n, want, err)
		}
	}
}

func TestLakebaseTelemetryCleanupDisabledIsolatedAcrossPoolsAndRegisteredDatabases(t *testing.T) {
	f := newHistoryWiringFixture(t)
	rt, owned, settings := f.registration(t)
	_, other, _ := f.registration(t)
	current, event := f.insertPlan(t, owned, f.now.Add(-time.Minute))
	foreign, _ := f.insertPlan(t, other, f.now.Add(-2*time.Minute))
	if err := f.store.Ack(f.ctx, current.Binding(), event.ID, "test_sink_v1"); err != nil {
		t.Fatal(err)
	}
	settings.Enabled = false
	if err := rt.cleanupLakebaseHistory(f.ctx, f.store, settings, f.now); err != nil {
		t.Fatal(err)
	}
	f.assertPayloadCopies(t, owned, current, 0)
	f.assertPayloadCopies(t, other, foreign, 1)
	var receipts, acknowledgements, misplacedCache int
	if err := f.control.QueryRow(f.ctx, "SELECT count(*) FROM sage.lakebaseobs_inbox WHERE binding=$1", current.Binding()).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if err := f.control.QueryRow(f.ctx, "SELECT count(*) FROM sage.lakebaseobs_acks WHERE binding=$1", current.Binding()).Scan(&acknowledgements); err != nil {
		t.Fatal(err)
	}
	if err := f.control.QueryRow(f.ctx, "SELECT count(*) FROM sage.explain_cache WHERE provider_scope_hash=$1", owned.Hash()).Scan(&misplacedCache); err != nil {
		t.Fatal(err)
	}
	if receipts != 1 || acknowledgements != 1 || misplacedCache != 0 {
		t.Fatalf("cleanup erased replay protection or used wrong cache pool: receipts=%d acks=%d misplaced=%d", receipts, acknowledgements, misplacedCache)
	}
}

func TestLakebaseTelemetryCleanupOutagePolicyRevocationAndEventAge(t *testing.T) {
	f := newHistoryWiringFixture(t)
	_, other, _ := f.registration(t)
	foreign, _ := f.insertPlan(t, other, f.now.Add(-2*time.Hour))
	for _, scenario := range []struct {
		name       string
		inMemory   bool
		change     func(*config.LakebaseObservabilityConfig)
		wantRecent int
		wantOld    int
	}{
		{"same-policy SDK outage preserves verified history", false, func(*config.LakebaseObservabilityConfig) {}, 1, 1},
		{"verified current scope retires prior database incarnation", true, func(*config.LakebaseObservabilityConfig) {}, 1, 0},
		{"configured endpoint change revokes before SDK recovery", false, func(c *config.LakebaseObservabilityConfig) { c.Bindings[0].Endpoint += "-changed" }, 0, 0},
		{"payload disabled erases both managed copies", false, func(c *config.LakebaseObservabilityConfig) { c.AllowPayload = false }, 0, 0},
		{"event-age reduction survives collection outage", false, func(c *config.LakebaseObservabilityConfig) { c.RetentionHours = 1 }, 1, 0},
		{"stale in-memory scope cannot authorize a changed binding", true, func(c *config.LakebaseObservabilityConfig) { c.Bindings[0].Endpoint += "-changed" }, 0, 0},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			rt, current, settings := f.registration(t)
			old := current
			old.DatabaseOID++ // Earlier verified native database incarnation.
			latest, _ := f.insertPlan(t, current, f.now.Add(-time.Minute))
			previous, _ := f.insertPlan(t, old, f.now.Add(-2*time.Hour))
			if err := latest.Poll(f.ctx, f.now); err == nil {
				t.Fatal("synthetic SDK outage unexpectedly returned success")
			}
			// The registry/runtime reader is unavailable. Cleanup must rely on
			// durable registered ownership and trusted policy, without SDK I/O.
			if scenario.inMemory {
				rt.lakebaseScope.Store(lakebaseVerifiedBinding{ScopeHash: current.Hash(), PolicyBinding: current.PolicyBinding})
			}
			scenario.change(&settings)
			if err := rt.cleanupLakebaseHistory(f.ctx, f.store, settings, f.now); err != nil {
				t.Fatal(err)
			}
			f.assertPayloadCopies(t, current, latest, scenario.wantRecent)
			f.assertPayloadCopies(t, old, previous, scenario.wantOld)
			f.assertPayloadCopies(t, other, foreign, 1)
		})
	}
}
