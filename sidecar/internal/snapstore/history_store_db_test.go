package snapstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/snapstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
	"github.com/pg-sage/sidecar/internal/testsupport/snapfixture"
)

// history.store: meta. The writer stores this database's rows in the
// store, tagged with its database id, with every delta's base inside the
// store and the same database; every document reads back exactly as in
// monitored mode.

func persistScenario(t *testing.T, p *histfixture.Pair, start time.Time) {
	t.Helper()
	sc := snapfixture.Scenario{Start: start,
		Step: time.Hour, Cycles: 18, Tables: 5, Indexes: 6, Seed: 7,
		Events: snapfixture.Events{DropIndex: 4, CounterReset: 9, QueryChurn: 3}}
	cycles, err := sc.Generate()
	if err != nil {
		t.Fatal(err)
	}
	w := snapstore.NewWriter()
	for _, c := range cycles {
		rows := make([]snapstore.Row, 0, len(c.Docs))
		for _, d := range c.Docs {
			rows = append(rows, snapstore.Row{Category: d.Category, Data: d.Data})
		}
		if err := w.Persist(context.Background(), p.Monitored, c.At, rows); err != nil {
			t.Fatalf("persist: %v", err)
		}
	}
}

func readBackStore(t *testing.T, p *histfixture.Pair) []string {
	t.Helper()
	rows, err := histstore.Resolve(p.Monitored).Query(context.Background(),
		`SELECT s.collected_at::text || ' ' || s.category || ' ' || `+
			snapstore.DataSQL("s")+`::text
		 FROM sage.snapshots s WHERE {db:s} ORDER BY s.collected_at, s.category`)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s *string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		if s == nil {
			t.Fatal("a row reads back as NULL: its base is missing")
		}
		out = append(out, *s)
	}
	return out
}

func TestWriterIdenticalInBothPlacements(t *testing.T) {
	p := histfixture.NewPair(t)
	got := map[histstore.Mode][]string{}
	start := time.Now().UTC().Add(-20 * time.Hour) // the same fixture in both placements
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		p.Noise(t, time.Now().Add(-10*time.Hour), 1)
		persistScenario(t, p, start)
		got[mode] = readBackStore(t, p)
	}
	mon, meta := got[histstore.ModeMonitored], got[histstore.ModeMeta]
	if len(mon) == 0 || len(mon) != len(meta) {
		t.Fatalf("monitored %d rows, meta %d rows", len(mon), len(meta))
	}
	for i := range mon {
		if mon[i] != meta[i] {
			t.Fatalf("row %d differs:\n%s\n%s", i, mon[i], meta[i])
		}
	}
	monRows, metaRows := p.Count(t, "snapshots")
	if monRows != 0 || metaRows != int64(len(meta)) {
		t.Fatalf("meta mode: %d monitored rows (want 0), %d store rows (want %d)",
			monRows, metaRows, len(meta))
	}
	var deltas, foreign int
	if err := p.Meta.QueryRow(context.Background(), `SELECT count(*),
		count(*) FILTER (WHERE b.database_id IS DISTINCT FROM d.database_id)
		FROM sage.snapshots d JOIN sage.snapshots b ON b.id = d.base_id
		WHERE d.database_id = $1`, histfixture.DatabaseID).Scan(&deltas, &foreign); err != nil {
		t.Fatal(err)
	}
	if deltas == 0 || foreign != 0 {
		t.Fatalf("%d deltas, %d built on another database's row", deltas, foreign)
	}
}

func TestWriterNeverFallsBackToTheMonitoredDatabase(t *testing.T) {
	p := histfixture.NewPair(t)
	down := histfixture.NewPair(t)
	store, err := histstore.NewMeta(down.Meta, histfixture.DatabaseID)
	if err != nil {
		t.Fatal(err)
	}
	down.Meta.Close()
	unreg := histstore.Register(p.Monitored, "app", store)
	defer unreg()
	err = snapstore.NewWriter().Persist(context.Background(), p.Monitored, time.Now(),
		[]snapstore.Row{{Category: "system", Data: []byte(`{"a":1}`)}})
	if err == nil {
		t.Fatal("a write to an unreachable store must fail")
	}
	if n, _ := p.Count(t, "snapshots"); n != 0 {
		t.Fatalf("the failed write landed %d rows in the monitored database", n)
	}
}
