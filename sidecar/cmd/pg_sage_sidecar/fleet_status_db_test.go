package main

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// cases-11 on real PostgreSQL: the status refresh counts unresolved
// incidents by severity, and an unreadable incidents table leaves the
// counts unknown while findings still refresh.

func incidentStatusPool(t *testing.T, label string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	p, err := pgxpool.New(ctx, testdb.CreateDatabase(t, label))
	if err != nil {
		t.Fatalf("connect %s: %v", label, err)
	}
	t.Cleanup(p.Close)
	if err := schema.Bootstrap(ctx, p); err != nil {
		t.Fatalf("bootstrap %s: %v", label, err)
	}
	return p
}

func seedIncident(t *testing.T, p *pgxpool.Pool, severity string, resolved bool) {
	t.Helper()
	_, err := p.Exec(context.Background(), `INSERT INTO sage.incidents
		(severity, root_cause, source, database_name, resolved_at)
		VALUES ($1, 'cases-11 probe', 'deterministic', 'probe',
		        CASE WHEN $2 THEN now() END)`, severity, resolved)
	if err != nil {
		t.Fatalf("seed %s incident: %v", severity, err)
	}
}

func TestUpdateInstanceFindings_CountsOpenIncidents(t *testing.T) {
	p := incidentStatusPool(t, "incident_counts")
	seedIncident(t, p, "critical", false)
	seedIncident(t, p, "critical", true)
	seedIncident(t, p, "warning", false)
	seedIncident(t, p, "warning", false)
	seedIncident(t, p, "info", false)
	inst := &fleet.DatabaseInstance{Name: "probe", Pool: p,
		Status: &fleet.InstanceStatus{IncidentsCritical: fleet.IncidentsUnknown,
			IncidentsWarning: fleet.IncidentsUnknown}}
	updateInstanceFindings(context.Background(), inst)
	got := inst.SnapshotStatus()
	if got.IncidentsCritical != 1 || got.IncidentsWarning != 2 {
		t.Fatalf("incidents critical %d warning %d, want 1 (resolved one excluded) and 2",
			got.IncidentsCritical, got.IncidentsWarning)
	}
	if !got.Connected || got.Error != "" {
		t.Fatalf("status after refresh: connected %v error %q", got.Connected, got.Error)
	}
}

func TestUpdateInstanceFindings_UnreadableIncidentsAreUnknown(t *testing.T) {
	p := incidentStatusPool(t, "incident_unreadable")
	ctx := context.Background()
	if _, err := p.Exec(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail, recommendation)
		VALUES ('probe', 'warning', 'table', 'public.t', 't', '{}', 'r')`); err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	if _, err := p.Exec(ctx, `ALTER TABLE sage.incidents RENAME TO incidents_moved`); err != nil {
		t.Fatalf("hide incidents: %v", err)
	}
	inst := &fleet.DatabaseInstance{Name: "probe", Pool: p,
		Status: &fleet.InstanceStatus{Connected: true}}
	updateInstanceFindings(ctx, inst)
	got := inst.SnapshotStatus()
	if got.IncidentsCritical != fleet.IncidentsUnknown ||
		got.IncidentsWarning != fleet.IncidentsUnknown {
		t.Fatalf("incidents critical %d warning %d, want unknown (-1)",
			got.IncidentsCritical, got.IncidentsWarning)
	}
	if got.FindingsWarning != 1 || !got.Connected {
		t.Fatalf("findings must still refresh: warning %d connected %v",
			got.FindingsWarning, got.Connected)
	}
}

func TestUpdateInstanceFindings_FindingsErrorLeavesIncidentsUnknown(t *testing.T) {
	p := incidentStatusPool(t, "incident_findings_err")
	seedIncident(t, p, "critical", false)
	ctx := context.Background()
	if _, err := p.Exec(ctx, `ALTER TABLE sage.findings RENAME TO findings_moved`); err != nil {
		t.Fatalf("hide findings: %v", err)
	}
	inst := &fleet.DatabaseInstance{Name: "probe", Pool: p,
		Status: &fleet.InstanceStatus{Connected: true, IncidentsCritical: 0}}
	updateInstanceFindings(ctx, inst)
	got := inst.SnapshotStatus()
	if got.IncidentsCritical != fleet.IncidentsUnknown {
		t.Fatalf("incidents critical %d after a failed refresh, want unknown (-1)",
			got.IncidentsCritical)
	}
}
