package lakebaseobs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

func evidenceFixture(t *testing.T, retention time.Duration) (*EvidenceSource, Scope, time.Time) {
	return evidenceModeFixture(t, retention, true)
}

func evidenceModeFixture(t *testing.T, retention time.Duration, payload bool) (*EvidenceSource, Scope, time.Time) {
	t.Helper()
	s, _ := dbStore(t)
	scope := testScope()
	scope.DeploymentID, scope.DatabaseID = uuid.NewString(), uuid.NewString()
	if err := s.pool.QueryRow(context.Background(), `SELECT current_database(),oid::bigint FROM pg_database WHERE datname=current_database()`).Scan(&scope.DatabaseName, &scope.DatabaseOID); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateOwner(context.Background(), scope, payload); err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	opts.Retention = retention
	opts.AllowPayload = payload
	r, err := NewRuntime(readerFunc(func(context.Context, Stream, Scope, Window) (Batch, error) { return Batch{}, nil }), s, scope, opts)
	if err != nil {
		t.Fatal(err)
	}
	source, err := NewEvidenceSource(s, r, s.pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM sage.lakebaseobs_inbox WHERE binding=$1`, r.Binding())
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM sage.lakebaseobs_cursors WHERE binding=$1`, r.Binding())
	})
	return source, scope, time.Now().UTC().Truncate(time.Microsecond)
}

func TestDBHistoricalMetadataPolicySupportsStructuredDiagnostics(t *testing.T) {
	s, scope, now := evidenceModeFixture(t, SourceRetention, false)
	for _, stream := range []Stream{Plans, Logs, ASH, DDL, DatabaseCounters, DatabaseGauges} {
		fields := map[string]string{}
		if stream == DDL {
			fields["command_tag"], fields["object_type"] = "ALTER TABLE", "table"
		}
		e := scopedEvent(t, scope, stream, now.Add(-time.Minute), now, fields)
		metadata, err := NewEvent(stream, scope, e.Fields, now, false)
		if err != nil {
			t.Fatal(err)
		}
		seedEvidence(t, s, stream, now, metadata)
	}
	for _, id := range probes.HistoricalIDs() {
		r := s.readAt(context.Background(), id, probes.Args{}, now)
		if !r.Status.Usable() || len(r.Rows) == 0 {
			t.Fatalf("metadata policy blocked %s: %+v", id, r)
		}
		if _, err := probes.HistoricalRows(r); err != nil {
			t.Fatalf("%s typed historical provenance rejected: %v", id, err)
		}
		if id == probes.LakebaseLogs && r.Rows[0]["rca_signal"] != "log_deadlock_detected" {
			t.Fatal("metadata SQLSTATE classification missing")
		}
		if id == probes.LakebasePlans && (r.Rows[0]["plan_payload_available"] != false || r.Rows[0]["plan_hash"] != nil) {
			t.Fatal("metadata plan invented a captured payload/hash")
		}
	}
}

func scopedEvent(t *testing.T, scope Scope, stream Stream, at, now time.Time, fields map[string]string) Event {
	t.Helper()
	f := testFields(stream, at)
	f["dbid"], f["datid"], f["datname"] = fmt.Sprint(scope.DatabaseOID), fmt.Sprint(scope.DatabaseOID), scope.DatabaseName
	for k, v := range fields {
		f[k] = v
	}
	e, err := NewEvent(stream, scope, f, now, true)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func seedEvidence(t *testing.T, s *EvidenceSource, stream Stream, now time.Time, events ...Event) {
	t.Helper()
	err := s.store.CommitBatch(context.Background(), s.runtime.binding, stream, Cursor{}, Cursor{LastPoll: now, ForwardThrough: now, GapCount: 2}, events, SourceRetention)
	if err != nil {
		t.Fatal(err)
	}
}

func TestDBHistoricalEvidenceNormalizesRCAChangesAndIntervalsWithoutRawPayload(t *testing.T) {
	s, scope, now := evidenceFixture(t, SourceRetention)
	at := now.Add(-2 * time.Hour)
	seedEvidence(t, s, Plans, now, scopedEvent(t, scope, Plans, at, now, map[string]string{"query_text": "SELECT 'raw-sensitive-secret'"}))
	seedEvidence(t, s, Logs, now, scopedEvent(t, scope, Logs, at, now, map[string]string{"message": "deadlock detected raw-sensitive-secret", "statement": "SELECT 'raw-sensitive-secret'"}))
	seedEvidence(t, s, ASH, now, scopedEvent(t, scope, ASH, at, now, map[string]string{"blocking_pid": "92", "wait_event_type": "Lock"}))
	seedEvidence(t, s, DDL, now, scopedEvent(t, scope, DDL, at, now, map[string]string{"command_tag": "ALTER TABLE", "object_type": "TABLE", "object_name": "raw-sensitive-secret", "usename": "raw-sensitive-secret"}))
	seedEvidence(t, s, DatabaseCounters, now, scopedEvent(t, scope, DatabaseCounters, at, now, map[string]string{"xact_commit": "12", "xact_rollback": "1"}))
	seedEvidence(t, s, DatabaseGauges, now, scopedEvent(t, scope, DatabaseGauges, at, now, map[string]string{"numbackends": "3"}))
	for _, id := range probes.HistoricalIDs() {
		r := s.readAt(context.Background(), id, probes.Args{}, now)
		if r.Status != probes.StatusOK || len(r.Rows) == 0 || r.History == nil || r.History.Complete || r.History.ExactMultiplicity || r.History.GapCount < 2 {
			t.Fatalf("%s: %+v", id, r)
		}
		b, _ := json.Marshal(r)
		if strings.Contains(string(b), "raw-sensitive-secret") {
			t.Fatalf("%s copied raw payload into long-lived evidence", id)
		}
		for _, row := range r.Rows {
			if row["scope_hash"] != scope.Hash() || row["compute_id"] != "compute-1" || row["occurred_at"] != at || row["historical_only"] != true {
				t.Fatalf("provenance lost: %+v", row)
			}
		}
		if id == probes.LakebaseLogs && r.Rows[0]["rca_signal"] != "log_deadlock_detected" {
			t.Fatalf("historical RCA classification missing: %+v", r.Rows)
		}
		if id == probes.LakebaseChanges && (r.Rows[0]["kind"] != "ddl" || r.Rows[0]["signature"] != "provider_observed") {
			t.Fatalf("source-aware DDL missing: %+v", r.Rows)
		}
		if id == probes.LakebaseDatabase && r.Rows[0]["slo_usable"] != false {
			t.Fatal("interval metrics became an SLO")
		}
	}
}

func TestDBHistoricalEvidenceRetentionQueryScopeAndRevocation(t *testing.T) {
	s, scope, now := evidenceFixture(t, time.Hour)
	seedEvidence(t, s, Plans, now, scopedEvent(t, scope, Plans, now.Add(-2*time.Hour), now, nil), scopedEvent(t, scope, Plans, now.Add(-time.Minute), now, nil))
	p := &probes.ReadPolicy{Version: 1, Scope: "database", Database: scope.DatabaseName, Binding: probes.PoolBinding(s.monitored), QueryID: 17}
	ctx := probes.WithReadPolicy(context.Background(), p)
	r := s.readAt(ctx, probes.LakebasePlans, probes.Args{QueryID: 17}, now)
	if len(r.Rows) != 1 || r.Rows[0]["occurred_at"] != now.Add(-time.Minute) {
		t.Fatalf("expired or unscoped row leaked: %+v", r)
	}
	if r := s.readAt(ctx, probes.LakebasePlans, probes.Args{QueryID: 18}, now); r.Reason != probes.ReasonPolicyDenied {
		t.Fatalf("query scope bypassed: %+v", r)
	}
	if r := s.readAt(ctx, probes.LakebaseLogs, probes.Args{}, now); r.Reason != probes.ReasonPolicyDenied {
		t.Fatalf("query-bound log read permitted: %+v", r)
	}
	if r := s.readAt(context.Background(), probes.LakebasePlans, probes.Args{QueryID: 18}, now); r.Status.Usable() || len(r.Rows) != 0 || r.History == nil || r.History.Complete {
		t.Fatalf("empty asynchronous read proved health: %+v", r)
	}
	// Revoke without cleanup: the direct read fence must stop retained data.
	if err := s.store.ActivateOwner(context.Background(), scope, false); err != nil {
		t.Fatal(err)
	}
	if r := s.readAt(context.Background(), probes.LakebasePlans, probes.Args{}, now); r.Reason != "history_policy_revoked" || len(r.Rows) != 0 {
		t.Fatalf("revoked payload remained readable: %+v", r)
	}
}

func TestDBHistoricalEvidenceRequiresCurrentPhysicalDatabaseIdentity(t *testing.T) {
	s, scope, now := evidenceFixture(t, SourceRetention)
	seedEvidence(t, s, Plans, now, scopedEvent(t, scope, Plans, now.Add(-time.Minute), now, nil))
	// Simulate the verified runtime retaining the old identity after recreation.
	s.runtime.scope.DatabaseOID++
	r := s.readAt(context.Background(), probes.LakebasePlans, probes.Args{}, now)
	if r.Status.Usable() || r.Reason != "history_database_identity_mismatch" || len(r.Rows) != 0 {
		t.Fatalf("old identity read retained evidence: %+v", r)
	}
	s.monitored.Close()
	r = s.readAt(context.Background(), probes.LakebasePlans, probes.Args{}, now)
	if r.Status != probes.StatusError || r.Reason != "history_identity_unavailable" {
		t.Fatalf("connection failure presented as policy denial: %+v", r)
	}
}

func TestHistoricalMetadataHandlesMaskedLogsAndRejectsPayloadTags(t *testing.T) {
	scope := testScope()
	now := time.Now().UTC()
	e := scopedEvent(t, scope, Logs, now, now, map[string]string{"message": "<REDACTED>", "sqlstate": "40P01", "severity": "ERROR"})
	r, err := historicalRow(e, 0)
	if err != nil || r["rca_signal"] != "log_deadlock_detected" || r["sqlstate"] != "40P01" {
		t.Fatalf("typed masked-log diagnostic lost: %+v %v", r, err)
	}
	e.Fields["severity"] = ""
	r, err = historicalRow(e, 0)
	if err != nil || r["sqlstate"] != "40P01" || r["rca_signal"] != nil {
		t.Fatalf("unknown severity was inferred: %+v %v", r, err)
	}
	e.Fields["sqlstate"], e.Fields["severity"] = "", "ERROR"
	r, err = historicalRow(e, 0)
	if err != nil || r["severity"] != "ERROR" || r["rca_signal"] != nil {
		t.Fatalf("unknown SQLSTATE was inferred: %+v %v", r, err)
	}
	e.Fields["sqlstate"], e.Fields["severity"] = "", ""
	if _, err := historicalRow(e, 0); err != ErrMasked {
		t.Fatalf("unknown masked log became diagnostic: %v", err)
	}
	e = scopedEvent(t, scope, DDL, now, now, map[string]string{"command_tag": "ALTER TABLE secret ADD sensitive text"})
	if _, err := historicalRow(e, 0); err != ErrMalformed {
		t.Fatalf("SQL payload escaped through command tag: %v", err)
	}
	e.Fields["command_tag"], e.Fields["object_type"] = "ALTER TABLE", "TABLE 'secret'"
	if _, err := historicalRow(e, 0); err != ErrMalformed {
		t.Fatalf("payload escaped through object type: %v", err)
	}
	e.Fields["object_type"], e.Fields["application_name"] = "TABLE", "pg_sage"
	if _, err := historicalRow(e, 0); err != ErrSelfStatement {
		t.Fatalf("self DDL was not excluded: %v", err)
	}
}
