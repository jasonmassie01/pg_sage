//go:build cgo

package impact

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// The reasoner's closed tools queries_touching, table_context and
// whatif_index, backed by the engine.

func evidenceIDs(ev []changereview.Evidence) []string {
	var ids []string
	for _, e := range ev {
		ids = append(ids, e.ID)
	}
	return ids
}

func verifyAll(t *testing.T, ev []changereview.Evidence) {
	t.Helper()
	for _, e := range ev {
		if !changereview.ValidEvidenceID(e.ID) || !e.Verify() {
			t.Errorf("evidence %s is invalid or does not verify", e.ID)
		}
	}
}

func TestQueriesTouchingAColumnOrATable(t *testing.T) {
	e := newEnv(t)
	got, err := e.eng.QueriesTouching(e.ctx, "rv.rv_orders", "email")
	if err != nil {
		t.Fatal(err)
	}
	verifyAll(t, got.Evidence)
	if !got.Known || got.Relation != "rv.rv_orders" ||
		!slices.Equal(evidenceIDs(got.Evidence), []string{queryEvidenceID(e.f.Q1.ID)}) {
		t.Fatalf("statements using email: %+v, want exactly Q1", got)
	}
	all, err := e.eng.QueriesTouching(e.ctx, "rv_orders", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{queryEvidenceID(e.f.Q1.ID), queryEvidenceID(e.f.Q2.ID),
		queryEvidenceID(e.f.Q3.ID)}
	if ids := evidenceIDs(all.Evidence); !all.Known || all.Relation != "rv.rv_orders" ||
		!slices.Equal(ids, want) {
		t.Fatalf("statements on rv_orders: %v, want Q1, Q2, Q3 busiest first %v", ids, want)
	}
	q := decode[queryData](t, all.Evidence[0])
	if q.Calls != e.f.Q1.Calls || !q.RateKnown {
		t.Fatalf("Q1 evidence %+v", q)
	}
}

func TestQueriesTouchingWithoutAWorkloadIsUnknown(t *testing.T) {
	e := newEnv(t)
	e.f.Exec("DROP EXTENSION pg_stat_statements")
	got, err := e.eng.QueriesTouching(e.ctx, "rv.rv_orders", "email")
	if err != nil {
		t.Fatal(err)
	}
	if got.Known || !strings.Contains(got.Reason, "pg_stat_statements") ||
		len(got.Evidence) != 1 || !strings.HasPrefix(got.Evidence[0].ID, "unknown:") {
		t.Fatalf("no pg_stat_statements: %+v, want unknown with its evidence", got)
	}
}

func TestToolsRefuseBadInput(t *testing.T) {
	e := newEnv(t)
	for _, rel := range []string{"", "  ", "a.b.c", strings.Repeat("x", 200)} {
		if _, err := e.eng.QueriesTouching(e.ctx, rel, ""); !errors.Is(err,
			changereview.ErrInvalid) {
			t.Errorf("QueriesTouching(%.20q) = %v, want ErrInvalid", rel, err)
		}
		if _, err := e.eng.TableContext(e.ctx, rel); !errors.Is(err, changereview.ErrInvalid) {
			t.Errorf("TableContext(%.20q) = %v, want ErrInvalid", rel, err)
		}
	}
	if _, err := e.eng.QueriesTouching(e.ctx, "rv.missing", ""); !errors.Is(err,
		changereview.ErrNotFound) {
		t.Errorf("unknown relation: %v, want ErrNotFound", err)
	}
	if _, err := e.eng.TableContext(e.ctx, "rv.missing"); !errors.Is(err,
		changereview.ErrNotFound) {
		t.Errorf("unknown relation: %v, want ErrNotFound", err)
	}
	for _, ddl := range []string{"", "DROP INDEX rv.rv_orders_created_idx",
		"CREATE INDEX a ON rv.rv_orders (id); CREATE INDEX b ON rv.rv_orders (id)",
		"CREATE INDEX ON rv.missing (id)", "CREAT INDEX"} {
		_, err := e.eng.WhatIfIndex(e.ctx, ddl, nil)
		if !errors.Is(err, changereview.ErrInvalid) && !errors.Is(err,
			changereview.ErrNotFound) {
			t.Errorf("WhatIfIndex(%q) = %v, want ErrInvalid or ErrNotFound", ddl, err)
		}
	}
	many := make([]string, 11)
	for i := range many {
		many[i] = "SELECT 1"
	}
	if _, err := e.eng.WhatIfIndex(e.ctx, "CREATE INDEX ON rv.rv_orders (status)",
		many); !errors.Is(err, changereview.ErrInvalid) {
		t.Errorf("11 statements: %v, want ErrInvalid", err)
	}
}

func TestTableContextIsCitable(t *testing.T) {
	e := newEnv(t)
	e.f.Exec("CREATE VIEW rv.order_status AS SELECT id, status FROM rv.rv_orders")
	ev, err := e.eng.TableContext(e.ctx, "rv.rv_orders")
	if err != nil {
		t.Fatal(err)
	}
	verifyAll(t, ev)
	ids := evidenceIDs(ev)
	for _, want := range []string{"table:rv.rv_orders", "index:rv.rv_orders_created_idx",
		"index:rv.rv_orders_pkey", "view:rv.order_status"} {
		if !slices.Contains(ids, want) {
			t.Errorf("table context %v lacks %s", ids, want)
		}
	}
	tbl := decode[tableData](t, ev[slices.Index(ids, "table:rv.rv_orders")])
	if tbl.RelTuples < 190000 || tbl.TotalBytes <= 0 || !tbl.RateKnown {
		t.Fatalf("table evidence %+v", tbl)
	}
}

func TestWhatIfIndexMeasuresTheGivenStatements(t *testing.T) {
	e := newEnv(t)
	ev, err := e.eng.WhatIfIndex(e.ctx, "CREATE INDEX CONCURRENTLY rv_orders_sc_idx ON "+
		"rv.rv_orders (status, created_at)", []string{"SELECT id FROM rv.rv_orders "+
		"WHERE status = $1 ORDER BY created_at LIMIT 50"})
	if err != nil {
		t.Fatal(err)
	}
	verifyAll(t, []changereview.Evidence{ev})
	w := decode[whatIfData](t, ev)
	if !strings.HasPrefix(ev.ID, "whatif:") || w.Method != "hypopg" || !w.Available ||
		w.ImprovementPct == nil || *w.ImprovementPct <= 0 || w.Measured != 1 {
		t.Fatalf("what-if %s %+v, want one statement measured with an improvement", ev.ID, w)
	}
	eng := e.engine(e.f.Pool, func(d *Deps) { d.WhatIf = unavailableWhatIf{t} })
	ev, err = eng.WhatIfIndex(e.ctx, "CREATE INDEX ON rv.rv_orders (status)", nil)
	if err != nil {
		t.Fatal(err)
	}
	if w := decode[whatIfData](t, ev); w.Available || w.ImprovementPct != nil {
		t.Fatalf("HypoPG unavailable: %+v", w)
	}
}
