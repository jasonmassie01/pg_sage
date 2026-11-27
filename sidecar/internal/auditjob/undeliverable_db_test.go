package auditjob

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/siem"
)

type undeliverableRow struct {
	ident, severity, title, objType string
	occurrences                     int
	detail                          map[string]any
}

func undeliverableRows(t *testing.T, pool *pgxpool.Pool) []undeliverableRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT object_identifier, severity,
		title, object_type, occurrence_count, detail::text FROM sage.findings
		WHERE category = $1 AND status = 'open' ORDER BY object_identifier`,
		UndeliverableCategory)
	if err != nil {
		t.Fatalf("read findings: %v", err)
	}
	defer rows.Close()
	var out []undeliverableRow
	for rows.Next() {
		var r undeliverableRow
		var detail string
		if err := rows.Scan(&r.ident, &r.severity, &r.title, &r.objType, &r.occurrences,
			&detail); err != nil {
			t.Fatalf("scan finding: %v", err)
		}
		if err := json.Unmarshal([]byte(detail), &r.detail); err != nil {
			t.Fatalf("detail %q: %v", detail, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read findings: %v", err)
	}
	return out
}

// A stubbed event raises one warning per sink and chain (on the source's
// database, the pool it is given), naming sink, source, chain and seq; a
// later stub on the same lane refreshes it, another sink gets its own.
func TestRaiseUndeliverable(t *testing.T) {
	pool := fresh(t, "auditjob_undeliverable", true)
	ctx := context.Background()
	u := siem.Undeliverable{Sink: "soc", Source: "orders", Chain: "action_log", Seq: 7,
		Hash: "abc123", Reason: "siem sink soc: receiver answered HTTP 413"}
	if err := RaiseUndeliverable(ctx, pool, u); err != nil {
		t.Fatalf("raise: %v", err)
	}
	got := undeliverableRows(t, pool)
	if len(got) != 1 {
		t.Fatalf("findings = %+v, want one", got)
	}
	f := got[0]
	if f.severity != "warning" || f.objType != "siem_sink" ||
		f.ident != "siem_sink:soc/action_log" || !strings.Contains(f.title, "action_log") ||
		!strings.Contains(f.title, "7") {
		t.Fatalf("finding = %+v", f)
	}
	if f.detail["sink"] != "soc" || f.detail["source"] != "orders" ||
		f.detail["chain"] != "action_log" || f.detail["seq"] != float64(7) ||
		f.detail["hash"] != "abc123" || !strings.Contains(f.detail["reason"].(string), "413") {
		t.Fatalf("detail = %v", f.detail)
	}
	u.Seq = 9
	if err := RaiseUndeliverable(ctx, pool, u); err != nil {
		t.Fatalf("raise again: %v", err)
	}
	u.Sink = "lake"
	if err := RaiseUndeliverable(ctx, pool, u); err != nil {
		t.Fatalf("raise for another sink: %v", err)
	}
	got = undeliverableRows(t, pool)
	if len(got) != 2 || got[1].ident != "siem_sink:soc/action_log" ||
		got[1].occurrences != 2 || got[1].detail["seq"] != float64(9) ||
		got[0].ident != "siem_sink:lake/action_log" {
		t.Fatalf("findings after more stubs = %+v", got)
	}
}

// A sink and a source whose names hold "pg_sage" still get their finding:
// the self-monitoring filter must not drop it while the cursor moves on.
func TestRaiseUndeliverableSinkNamedLikeTheProduct(t *testing.T) {
	pool := fresh(t, "auditjob_undeliverable_named", true)
	u := siem.Undeliverable{Sink: "pg_sage_soc", Source: "pg_sage_demo", Chain: "action_log",
		Seq: 3, Hash: "h3", Reason: "siem sink pg_sage_soc: receiver answered HTTP 400"}
	if err := RaiseUndeliverable(context.Background(), pool, u); err != nil {
		t.Fatalf("raise: %v", err)
	}
	got := undeliverableRows(t, pool)
	if len(got) != 1 || got[0].ident != "siem_sink:pg_sage_soc/action_log" ||
		got[0].detail["sink"] != "pg_sage_soc" || got[0].detail["seq"] != float64(3) ||
		got[0].detail["source"] != "pg_sage_demo" {
		t.Fatalf("findings = %+v, want one for sink pg_sage_soc", got)
	}
	var listed int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.findings
		WHERE category = $1`+selfmonitor.FindingsSQLExclusionClause(),
		UndeliverableCategory).Scan(&listed); err != nil || listed != 1 {
		t.Fatalf("findings list shows %d undeliverable findings (%v), want 1", listed, err)
	}
}

// Without a database the report fails, so the exporter holds the cursor.
func TestRaiseUndeliverableErrors(t *testing.T) {
	err := RaiseUndeliverable(context.Background(), nil, siem.Undeliverable{Sink: "soc",
		Source: "orders", Chain: "action_log", Seq: 1})
	if err == nil || !strings.Contains(err.Error(), "orders") {
		t.Fatalf("nil pool: err = %v, want an error naming the source", err)
	}
	pool := fresh(t, "auditjob_undeliverable_bare", false)
	err = RaiseUndeliverable(context.Background(), pool, siem.Undeliverable{Sink: "soc",
		Source: "orders", Chain: "action_log", Seq: 1})
	if err == nil || !strings.Contains(err.Error(), UndeliverableCategory) {
		t.Fatalf("no findings table: err = %v, want an error naming the category", err)
	}
}
