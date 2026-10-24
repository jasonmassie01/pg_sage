package receipt

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/testsupport/rvfixture"
)

// CHECK-RCP-07: statements first seen in query_store after landing that
// read a touched relation are listed in the receipt as born queryids, with
// their post-landing latency and no regression verdict: they are what the
// review could not watch (an ORM statement it missed, say).

// runStatement runs text with args over the extended protocol and returns
// its pg_stat_statements queryid.
func runStatement(t *testing.T, rv *rvfixture.Fixture, text string, args ...any) int64 {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, rv.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for i := 0; i < 3; i++ {
		rows, err := conn.Query(ctx, text, args...)
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		rows.Close()
	}
	return rv.Int64(`SELECT queryid FROM pg_stat_statements s JOIN pg_database d
		ON d.oid = s.dbid WHERE d.datname = current_database() AND s.query = $1`, text)
}

func TestReceiptListsBornQueries(t *testing.T) {
	rv := rvfixture.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	f := newFixtureOn(t, ctx, rv.Pool, rv.Name)
	born := runStatement(t, rv, "SELECT status FROM rv.rv_orders WHERE customer_id = $1",
		int32(7))
	elsewhere := runStatement(t, rv, "SELECT qty FROM rv.rv_items WHERE order_id = $1",
		int32(7))
	at := time.Now().Add(-25 * time.Hour).UTC().Truncate(time.Microsecond)
	r := f.seed(func(r *changereview.Review) {
		landedAt(r, changereview.LandingSourceReported, at, at)
		watching(r, []string{rvfixture.Orders}, rv.Q1.ID)
	})
	f.beforeAfter(rv.Q1.ID, at, at, 10, 0.2, 0.2)
	last := f.series(born, at.Add(90*time.Minute), 4, 30*time.Minute, 20, 4, counters{})
	f.series(elsewhere, at.Add(time.Hour), 4, 30*time.Minute, 20, 1, counters{})
	// Q2 reads rv_orders too but ran before landing: not born.
	f.beforeAfter(rv.Q2.ID, at, at, 10, 0.3, 0.3)
	f.run()
	got := f.get(r.ID)
	if got.Receipt == nil || got.ReceiptVerdict != changereview.ReceiptClean {
		t.Fatalf("receipt %s, want clean: a born query does not regress", got.ReceiptVerdict)
	}
	if len(got.Receipt.BornQueryIDs) != 1 {
		t.Fatalf("born %+v, want only the new rv_orders statement", got.Receipt.BornQueryIDs)
	}
	b := got.Receipt.BornQueryIDs[0]
	if b.QueryID != born || !b.FirstSeen.Equal(at.Add(90*time.Minute)) ||
		b.Calls != last.calls || math.Abs(b.MeanMs-last.total/float64(last.calls)) > 1e-6 {
		t.Fatalf("born query %+v, want %d first seen %s with %d calls", b, born,
			at.Add(90*time.Minute), last.calls)
	}
	if len(b.Relations) != 1 || b.Relations[0] != rvfixture.Orders {
		t.Fatalf("born relations %v, want [%s]", b.Relations, rvfixture.Orders)
	}
	for _, tg := range got.Receipt.Targets {
		if tg.QueryID == born {
			t.Fatal("the born query was given a regression verdict")
		}
	}
}
