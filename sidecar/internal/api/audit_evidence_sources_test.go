package api

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/evidence"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// audit-07: in fleet mode the evidence route reads a database's sign-ins
// and config changes from the control database, by the database's id.
func TestAuditEvidenceReadsControlInFleet(t *testing.T) {
	monitored := auditPool(t, "api_audit_src_m")
	control := auditPool(t, "api_audit_src_c")
	ctx := context.Background()
	_, err := control.Exec(ctx, `INSERT INTO sage.config_audit (key, old_value,
		new_value, database_id) VALUES ('trust.level', 'a', 'orders-change', 7),
		('trust.level', 'a', 'billing-change', 8)`)
	require.NoError(t, err)
	deps := AuditDeps{Control: control,
		Pool: func(name string) *pgxpool.Pool {
			switch name {
			case "orders":
				return monitored
			case "control":
				return control
			}
			return nil
		},
		DatabaseID: func(name string) (int, bool) { return 7, name == "orders" }}
	mux := http.NewServeMux()
	registerAuditRoutes(mux, deps)
	m := windowManifest(t, mux, "orders")
	sources := map[string]string{}
	rows := map[string]int{}
	for _, f := range m.Files {
		sources[f.Name] = f.Source
		if f.Rows != nil {
			rows[f.Name] = *f.Rows
		}
	}
	require.Equal(t, "control", sources["config_audit.json"])
	require.Equal(t, "control", sources["auth_audit.json"])
	require.Equal(t, "database", sources["actions.json"])
	require.Equal(t, 1, rows["config_audit.json"])

	// The control pack itself reads everything from the control database.
	m = windowManifest(t, mux, "control")
	for _, f := range m.Files {
		if f.Name == "config_audit.json" {
			require.Equal(t, "database", f.Source)
			require.Equal(t, 2, *f.Rows)
		}
		if f.Source == "control" {
			t.Fatalf("control pack file %s labelled control", f.Name)
		}
	}
	require.True(t, strings.HasPrefix(m.Format, "pg_sage-evidence/2"))
}

// windowManifest downloads the last hour's pack of database and returns
// its verified manifest.
func windowManifest(t *testing.T, mux http.Handler, database string) evidence.Manifest {
	t.Helper()
	from := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	to := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	w := auditGet(mux, "/api/v1/audit/evidence?database="+database+"&from="+from+
		"&to="+to, true)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	m, err := evidence.Verify(bytes.NewReader(w.Body.Bytes()), nil)
	require.NoError(t, err)
	return m
}
