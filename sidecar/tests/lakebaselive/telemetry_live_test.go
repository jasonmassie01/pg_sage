package lakebaselive

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/databricksauth"
	"github.com/pg-sage/sidecar/internal/lakebaseobs"
)

type telemetryStreamResult struct {
	Stream                     lakebaseobs.Stream `json:"stream"`
	Rows                       int                `json:"rows"`
	Valid                      int                `json:"valid"`
	Masked                     int                `json:"masked"`
	Rejected                   int                `json:"rejected"`
	Status                     string             `json:"status"`
	StatementID                string             `json:"statement_id,omitempty"`
	LatestEvent                *time.Time         `json:"latest_event,omitempty"`
	MaxObservedDeliverySeconds *float64           `json:"max_observed_delivery_seconds,omitempty"`
}
type telemetryNativeReport struct {
	StartedAt              time.Time                `json:"started_at"`
	FinishedAt             time.Time                `json:"finished_at"`
	Scope                  lakebaseobs.Scope        `json:"scope"`
	Profile                string                   `json:"profile"`
	SyntheticApplication   string                   `json:"synthetic_application"`
	StartupApplication     bool                     `json:"startup_application"`
	SyntheticBackendPID    int32                    `json:"synthetic_backend_pid"`
	VerifiedApplication    string                   `json:"verified_session_application"`
	SyntheticQueryID       int64                    `json:"synthetic_query_id"`
	SyntheticQueryIDStatus string                   `json:"synthetic_query_id_status"`
	ExpectedSQLState       string                   `json:"expected_sqlstate"`
	Flags                  map[string]string        `json:"flags"`
	Metadata               []telemetryStreamResult  `json:"metadata"`
	SyntheticPayload       []telemetryStreamResult  `json:"synthetic_payload"`
	MetadataFinal          []telemetryStreamResult  `json:"metadata_final,omitempty"`
	RoutingDiagnostics     []telemetryRoutingResult `json:"routing_diagnostics,omitempty"`
	WaitSeconds            int                      `json:"wait_seconds"`
	PollAttempts           int                      `json:"poll_attempts"`
	KeepaliveExecutions    int                      `json:"keepalive_executions"`
	OwnedSchema            string                   `json:"owned_schema"`
	CleanupVerified        bool                     `json:"cleanup_verified"`
	ProviderCompleteness   string                   `json:"provider_completeness"`
}

// TestLakebaseTelemetry distinguishes verified scoped SDK/schema reads from
// actual event delivery. No customer query/plan/log payload is written or logged.
// Payload requests narrow to this run's exact external application identity.
func TestLakebaseTelemetry(t *testing.T) {
	if os.Getenv("PG_SAGE_LIVE_LAKEBASE_TELEMETRY") != "1" {
		t.Skip("set PG_SAGE_LIVE_LAKEBASE_TELEMETRY=1 for controlled telemetry acceptance")
	}
	warehouse := os.Getenv("SAGE_LAKEBASE_TELEMETRY_WAREHOUSE_ID")
	if warehouse == "" {
		t.Fatal("SAGE_LAKEBASE_TELEMETRY_WAREHOUSE_ID required")
	}
	wait := 30 * time.Second
	if raw := os.Getenv("SAGE_LAKEBASE_TELEMETRY_WAIT_SECONDS"); raw != "" {
		seconds, e := strconv.Atoi(raw)
		if e != nil || seconds < 0 || seconds > 1200 {
			t.Fatal("telemetry wait must be 0..1200 seconds")
		}
		wait = time.Duration(seconds) * time.Second
	}
	p, _ := livePool(t)
	// Native SQL fixtures usually need five minutes; the explicit telemetry
	// gate may wait longer because asynchronous delivery has no lag SLA.
	ctx, cancel := context.WithTimeout(context.Background(), wait+5*time.Minute)
	defer cancel()
	resource, err := databricksauth.ResolveTelemetryResource(ctx, p.Config().ConnConfig.Host, os.Getenv("SAGE_LAKEBASE_ENDPOINT"))
	if err != nil {
		t.Fatal(err)
	}
	var oid int64
	if err = p.QueryRow(ctx, "SELECT oid::bigint FROM pg_catalog.pg_database WHERE datname=current_database()").Scan(&oid); err != nil {
		t.Fatal(err)
	}
	scope := lakebaseobs.Scope{DeploymentID: uuid.NewString(), DatabaseID: uuid.NewString(), Workspace: resource.Workspace, ProjectID: resource.ProjectID, BranchID: resource.BranchID, EndpointID: resource.EndpointID, DatabaseName: "pg_sage_test", DatabaseOID: oid, AllowCompute: true}
	if err = scope.Validate(); err != nil {
		t.Fatal(err)
	}
	report := telemetryNativeReport{StartedAt: time.Now().UTC(), Scope: scope, Profile: os.Getenv("DATABRICKS_CONFIG_PROFILE"), Flags: map[string]string{}, WaitSeconds: int(wait / time.Second), ProviderCompleteness: "unknown; asynchronous Beta capture has no completeness or ingestion-lag SLA"}
	t.Cleanup(func() {
		report.FinishedAt = time.Now().UTC()
		path := os.Getenv("SAGE_LAKEBASE_TELEMETRY_REPORT")
		if path == "" {
			return
		}
		raw, e := json.MarshalIndent(report, "", "  ")
		if e != nil {
			t.Error(e)
			return
		}
		if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
			t.Error(e)
			return
		}
		if e = os.WriteFile(path, append(raw, '\n'), 0600); e != nil {
			t.Error(e)
		}
	})
	for _, setting := range []string{"databricks.o11y_exporter_enabled", "neon_monitor.log_capture_enabled", "neon_monitor.plan_enabled", "neon_monitor.log_capture_min_level", "auto_explain.log_min_duration"} {
		var value string
		if err = p.QueryRow(ctx, "SELECT current_setting($1,true)", setting).Scan(&value); err != nil {
			t.Fatal(err)
		}
		report.Flags[setting] = value
	}
	t.Logf("verified API scope project=%s branch=%s endpoint=%s database_oid=%d; exporter=%s capture=%s plans=%s minimum_log_level=%s", scope.ProjectID, scope.BranchID, scope.EndpointID, scope.DatabaseOID, report.Flags["databricks.o11y_exporter_enabled"], report.Flags["neon_monitor.log_capture_enabled"], report.Flags["neon_monitor.plan_enabled"], report.Flags["neon_monitor.log_capture_min_level"])
	// A task-owned session, isolated owned DDL and a safe statement timeout generate
	// synthetic signals without changing endpoint/provider configuration.
	// The observed provider plan/DDL application_name field retains 31 bytes,
	// while PostgreSQL and log capture retain the full session value. Keep this
	// exact synthetic selector below that boundary; never broaden payload reads
	// to a shared truncated prefix. The owned schema still uses a full UUID.
	tag := "lbo_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
	report.SyntheticApplication = tag
	fixturePool := p
	if os.Getenv("SAGE_LAKEBASE_TELEMETRY_STARTUP_APPLICATION") == "1" {
		cfg := p.Config()
		cfg.ConnConfig.RuntimeParams["application_name"] = tag
		fixturePool, err = pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(fixturePool.Close)
		report.StartupApplication = true
	}
	conn, err := fixturePool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if !report.StartupApplication {
		if _, err = conn.Exec(ctx, "SELECT set_config('application_name',$1,false)", tag); err != nil {
			t.Fatal(err)
		}
		defer conn.Exec(context.Background(), "SELECT set_config('application_name','pg_sage_lakebase_live_tests',false)")
	}
	if err = conn.QueryRow(ctx, "SELECT pg_backend_pid(),current_setting('application_name')").Scan(&report.SyntheticBackendPID, &report.VerifiedApplication); err != nil || report.VerifiedApplication != tag {
		t.Fatal("owned session application not verified", err)
	}
	report.OwnedSchema = "sage_telemetry_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedSchema := pgx.Identifier{report.OwnedSchema}.Sanitize()
	if _, err = conn.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if _, cleanupErr := p.Exec(cleanup, "DROP SCHEMA "+quotedSchema+" CASCADE"); cleanupErr != nil {
			t.Errorf("owned telemetry fixture cleanup failed: %v", cleanupErr)
			return
		}
		var exists bool
		if cleanupErr := p.QueryRow(cleanup, "SELECT EXISTS(SELECT FROM pg_namespace WHERE nspname=$1)", report.OwnedSchema).Scan(&exists); cleanupErr != nil || exists {
			t.Errorf("owned telemetry fixture cleanup not verified: %v", cleanupErr)
			return
		}
		report.CleanupVerified = true
		t.Log("owned telemetry schema cleanup verified")
	})
	if _, err = conn.Exec(ctx, "CREATE TABLE "+quotedSchema+".owned_fixture(id integer)"); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, "INSERT INTO "+quotedSchema+".owned_fixture VALUES (42)"); err != nil {
		t.Fatal(err)
	}
	query := "/* " + tag + " */ SELECT id FROM " + quotedSchema + ".owned_fixture CROSS JOIN pg_sleep(1.2)"
	for range 2 {
		var value int
		if err = conn.QueryRow(ctx, query).Scan(&value); err != nil || value != 42 {
			t.Fatal("controlled workload failed", err)
		}
	}
	_ = conn.QueryRow(ctx, `SELECT queryid FROM pg_stat_statements WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database()) AND query LIKE $1 ORDER BY calls DESC LIMIT 1`, "/* "+tag+" */%").Scan(&report.SyntheticQueryID)
	report.SyntheticQueryIDStatus = "unavailable_from_normalized_representative_text"
	if report.SyntheticQueryID != 0 {
		report.SyntheticQueryIDStatus = "verified_owned_pg_stat_statements_entry"
	}
	// EXPLAIN is plan-only for our own synthetic query. Keep its plan payload
	// exclusively in memory; retain only the query identifier when supplied.
	var ownExplain []byte
	if err = conn.QueryRow(ctx, "EXPLAIN (VERBOSE, FORMAT JSON) "+query).Scan(&ownExplain); err == nil {
		var docs []struct {
			ID json.Number `json:"Query Identifier"`
		}
		if json.Unmarshal(ownExplain, &docs) == nil && len(docs) == 1 && docs[0].ID != "" {
			if id, e := strconv.ParseInt(string(docs[0].ID), 10, 64); e == nil {
				report.SyntheticQueryID = id
				report.SyntheticQueryIDStatus = "verified_owned_explain_identifier"
			} else if id, e := strconv.ParseUint(string(docs[0].ID), 10, 64); e == nil {
				report.SyntheticQueryID = int64(id)
				report.SyntheticQueryIDStatus = "verified_owned_explain_identifier"
			}
		}
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "SET LOCAL statement_timeout='20ms'"); err != nil {
		t.Fatal(err)
	}
	_, timeoutErr := tx.Exec(ctx, "SELECT 42 FROM pg_sleep(0.1)")
	_ = tx.Rollback(ctx)
	var pgErr *pgconn.PgError
	if !errors.As(timeoutErr, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("controlled timeout did not yield expected SQLSTATE: %T", timeoutErr)
	}
	report.ExpectedSQLState = pgErr.Code
	api, err := databricksauth.StatementExecution()
	if err != nil {
		t.Fatal(err)
	}
	streams := []lakebaseobs.Stream{lakebaseobs.Plans, lakebaseobs.Logs, lakebaseobs.ASH, lakebaseobs.DDL, lakebaseobs.ComputeCounters, lakebaseobs.ComputeGauges, lakebaseobs.DatabaseCounters, lakebaseobs.DatabaseGauges, lakebaseobs.WaitCounters, lakebaseobs.StatementCounters}
	options := lakebaseobs.Options{WarehouseID: warehouse, AllowPayload: false, MaxRows: 1000, MaxBytes: 4 << 20, QueryTimeout: 20 * time.Second, Streams: streams}
	metadata, err := lakebaseobs.NewSDKReader(api, options)
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range streams {
		t.Run("ScopedSchema_"+string(stream), func(t *testing.T) {
			batch, readErr := metadata.ReadWindow(ctx, stream, scope, lakebaseobs.Window{From: report.StartedAt.Add(-time.Minute), To: time.Now().UTC()})
			result := telemetryStreamResult{Stream: stream, Rows: len(batch.Events), StatementID: batch.QueryID, Status: "scoped_schema_read_verified_delivery_unestablished"}
			if readErr != nil {
				result.Status = readErr.Error()
				report.Metadata = append(report.Metadata, result)
				t.Error(readErr)
				return
			}
			report.Metadata = append(report.Metadata, result)
			t.Logf("resource-scoped metadata rows=%d statement=%s; empty data does not establish capture completeness", len(batch.Events), batch.QueryID)
		})
	}
	options.AllowPayload = true
	payload, err := lakebaseobs.NewSDKReader(api, options)
	if err != nil {
		t.Fatal(err)
	}
	scope.ApplicationName = tag
	report.RoutingDiagnostics = diagnoseTelemetryRouting(ctx, api, warehouse, scope, tag, report.StartedAt)
	for _, diagnostic := range report.RoutingDiagnostics {
		t.Logf("initial routing stream=%s selector=%s rows=%v error=%s", diagnostic.Stream, diagnostic.Selector, diagnostic.Rows, diagnostic.Error)
	}
	deadline := time.Now().Add(wait)
	results := map[lakebaseobs.Stream]telemetryStreamResult{}
	for {
		report.PollAttempts++
		// Keep the explicitly owned compute active throughout the lag window.
		// Warehouse reads alone would allow its five-minute suspension timer.
		var keepalive int
		if err := conn.QueryRow(ctx, "SELECT 42").Scan(&keepalive); err != nil || keepalive != 42 {
			t.Fatalf("owned compute keepalive failed: %v", err)
		}
		report.KeepaliveExecutions++
		for _, stream := range []lakebaseobs.Stream{lakebaseobs.Plans, lakebaseobs.Logs, lakebaseobs.DDL} {
			if results[stream].Valid > 0 {
				continue
			}
			b, e := payload.ReadWindow(ctx, stream, scope, lakebaseobs.Window{From: report.StartedAt.Add(-time.Second), To: time.Now().UTC()})
			r := telemetryStreamResult{Stream: stream, Rows: len(b.Events), StatementID: b.QueryID, Status: "no_synthetic_delivery_observed"}
			if e != nil {
				r.Status = e.Error()
				t.Errorf("synthetic %s read: %v", stream, e)
				results[stream] = r
				continue
			}
			for _, event := range b.Events {
				var normalizeErr error
				switch stream {
				case lakebaseobs.Plans:
					_, normalizeErr = lakebaseobs.NormalizePlan(event)
				case lakebaseobs.Logs:
					_, normalizeErr = lakebaseobs.NormalizeLog(event)
				case lakebaseobs.DDL:
					_, normalizeErr = lakebaseobs.NormalizeDDL(event)
				}
				if errors.Is(normalizeErr, lakebaseobs.ErrMasked) {
					r.Masked++
				} else if normalizeErr != nil {
					r.Rejected++
				} else {
					r.Valid++
					at := event.OccurredAt
					if r.LatestEvent == nil || at.After(*r.LatestEvent) {
						r.LatestEvent = &at
					}
					lag := event.ReceivedAt.Sub(at).Seconds()
					if r.MaxObservedDeliverySeconds == nil || lag > *r.MaxObservedDeliverySeconds {
						r.MaxObservedDeliverySeconds = &lag
					}
				}
			}
			if r.Valid > 0 {
				r.Status = "controlled_synthetic_delivery_verified"
			}
			results[stream] = r
		}
		if report.PollAttempts == 1 || report.PollAttempts%5 == 0 {
			t.Logf("delivery attempt=%d elapsed=%s plans=%d logs=%d ddl=%d", report.PollAttempts, time.Since(report.StartedAt).Round(time.Second), results[lakebaseobs.Plans].Valid, results[lakebaseobs.Logs].Valid, results[lakebaseobs.DDL].Valid)
		}
		if !time.Now().Before(deadline) || (results[lakebaseobs.Plans].Valid > 0 && results[lakebaseobs.Logs].Valid > 0 && results[lakebaseobs.DDL].Valid > 0) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(15 * time.Second):
		}
	}
	// Recheck every metadata source after the bounded lag window; early empty
	// reads are never reused as evidence that later delivery is healthy.
	scope.ApplicationName = ""
	for _, stream := range streams {
		b, readErr := metadata.ReadWindow(ctx, stream, scope, lakebaseobs.Window{From: report.StartedAt.Add(-time.Minute), To: time.Now().UTC()})
		r := telemetryStreamResult{Stream: stream, Rows: len(b.Events), StatementID: b.QueryID, Status: "selected_resource_delivery_observed"}
		if readErr != nil {
			r.Status = readErr.Error()
		} else if len(b.Events) == 0 {
			r.Status = "selected_resource_delivery_absent_after_bounded_wait"
		}
		report.MetadataFinal = append(report.MetadataFinal, r)
		t.Run("MetadataDelivery_"+string(stream), func(t *testing.T) {
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(b.Events) == 0 && os.Getenv("SAGE_LAKEBASE_TELEMETRY_REQUIRE_DELIVERY") == "1" {
				t.Error("observed provider delivery gap: no selected-resource rows after bounded active-compute wait")
			}
		})
	}
	report.RoutingDiagnostics = append(report.RoutingDiagnostics, diagnoseTelemetryRouting(ctx, api, warehouse, scope, tag, report.StartedAt)...)
	for _, stream := range []lakebaseobs.Stream{lakebaseobs.Plans, lakebaseobs.Logs, lakebaseobs.DDL} {
		r := results[stream]
		report.SyntheticPayload = append(report.SyntheticPayload, r)
		t.Run("ControlledDelivery_"+string(stream), func(t *testing.T) {
			t.Logf("rows=%d usable=%d masked=%d rejected=%d status=%s", r.Rows, r.Valid, r.Masked, r.Rejected, r.Status)
			if r.Valid == 0 {
				if os.Getenv("SAGE_LAKEBASE_TELEMETRY_REQUIRE_DELIVERY") == "1" {
					t.Fatal("native telemetry functional promotion blocked: no usable controlled event")
				}
				t.Skip("native event delivery unverified; inspect resource mapping/capture/exporter routing without private settings")
			}
		})
	}
}
