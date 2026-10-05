package testdb

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// A batch (pgx pipeline) is one round trip however many statements it
// carries: the recorder keeps each statement for Matching and counts the
// batch once in RoundTrips.
func TestQueryRecorder_BatchesAreOneRoundTrip(t *testing.T) {
	rec := &QueryRecorder{}
	ctx := context.Background()
	rec.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "UPDATE sage.a SET x = 1"})
	rec.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	bctx := rec.TraceBatchStart(ctx, nil, pgx.TraceBatchStartData{})
	if bctx != ctx {
		t.Fatal("TraceBatchStart replaced the context")
	}
	args := []any{7}
	rec.TraceBatchQuery(bctx, nil, pgx.TraceBatchQueryData{SQL: "SELECT 1 FROM sage.b",
		Args: args})
	args[0] = 99
	rec.TraceBatchQuery(bctx, nil, pgx.TraceBatchQueryData{SQL: "SELECT 2 FROM sage.c"})
	rec.TraceBatchEnd(bctx, nil, pgx.TraceBatchEndData{})
	if n := rec.RoundTrips(); n != 2 {
		t.Fatalf("round trips = %d, want 2 (one statement, one batch)", n)
	}
	got := rec.Matching("sage.b")
	if len(got) != 1 || got[0].Args[0] != 7 {
		t.Fatalf("batched statement = %+v, want it recorded with its own args", got)
	}
	if n := len(rec.Matching()); n != 3 {
		t.Fatalf("statements = %d, want 3", n)
	}
	rec.Reset()
	if rec.RoundTrips() != 0 || len(rec.Matching()) != 0 {
		t.Fatal("Reset must clear statements and round trips")
	}
}

func TestQueryRecorder_EmptyBatchStillCountsOnce(t *testing.T) {
	rec := &QueryRecorder{}
	ctx := context.Background()
	rec.TraceBatchStart(ctx, nil, pgx.TraceBatchStartData{})
	rec.TraceBatchEnd(ctx, nil, pgx.TraceBatchEndData{})
	if rec.RoundTrips() != 1 || len(rec.Matching()) != 0 {
		t.Fatalf("round trips %d, statements %d", rec.RoundTrips(), len(rec.Matching()))
	}
}
