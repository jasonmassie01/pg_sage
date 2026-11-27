package selfcost

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// The per-table rows are the one size source: their sum is the schema's
// size, their statistics the rows read and written, largest first.
func TestReadSchema_SumsTheTables(t *testing.T) {
	db := newFakeDB("1.11")
	db.tables = []fakeTable{{"sage.snapshots", 9000, 40, 7}, {"sage.findings", 1000, 2, 3}}
	sc, err := ReadSchema(context.Background(), db)
	if err != nil {
		t.Fatalf("ReadSchema: %v", err)
	}
	if sc.Database != "app" || sc.Bytes != 10000 || sc.RowsRead != 42 || sc.RowsWritten != 10 {
		t.Fatalf("schema = %+v, want app, 10000 bytes, 42 read, 10 written", sc)
	}
	if len(sc.Tables) != 2 || sc.Tables[0] != (TableSize{"sage.snapshots", 9000}) ||
		sc.Tables[1] != (TableSize{"sage.findings", 1000}) {
		t.Fatalf("tables = %+v, want both, largest first", sc.Tables)
	}
	if db.count("schema") != 1 {
		t.Fatalf("schema read issued %d statements, want 1", db.count("schema"))
	}
}

// Nil/empty: no sage table (one row of NULLs) is a size of 0 with the
// database named, not an error.
func TestReadSchema_EmptySchemaIsZero(t *testing.T) {
	db := newFakeDB("1.11")
	sc, err := ReadSchema(context.Background(), db)
	if err != nil || sc.Database != "app" || sc.Bytes != 0 || len(sc.Tables) != 0 ||
		sc.RowsRead != 0 || sc.RowsWritten != 0 {
		t.Fatalf("empty schema = %+v, err %v", sc, err)
	}
}

// Error propagation: a failed or empty read is an error naming the sage
// schema, keeping the server's code.
func TestReadSchema_Errors(t *testing.T) {
	db := newFakeDB("1.11")
	db.schemaErr = &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock"}
	_, err := ReadSchema(context.Background(), db)
	var pgErr *pgconn.PgError
	if err == nil || !strings.Contains(err.Error(), "read sage schema") ||
		!errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("lock timeout: err %v", err)
	}
	db = newFakeDB("1.11")
	db.noSchemaRow = true
	if _, err := ReadSchema(context.Background(), db); err == nil ||
		!strings.Contains(err.Error(), "read sage schema") {
		t.Fatalf("no row: err %v, want an error", err)
	}
}

// A statement failure leaves the schema counters: the reading keeps them,
// with the statements unknown and the failure reported beside it.
func TestSample_StatementFailureKeepsSchema(t *testing.T) {
	db := newFakeDB("1.11")
	db.countersErr = &pgconn.PgError{Code: "57014"}
	sc := Schema{Database: "app", Bytes: 4096, RowsRead: 5, RowsWritten: 6}
	r := NewSampler(nil).Sample(context.Background(), db, sc)
	if r.StatementsKnown || r.StatementsErr == nil ||
		!strings.Contains(r.StatementsErr.Error(), "read pg_sage statements") {
		t.Fatalf("statement failure: known=%v err=%v", r.StatementsKnown, r.StatementsErr)
	}
	if r.Database != "app" || r.SchemaBytes != 4096 || r.RowsRead != 5 ||
		r.RowsWritten != 6 || r.At.IsZero() {
		t.Fatalf("schema counters lost: %+v", r)
	}
}

// The reading sums pg_sage's statements and leaves the others out.
func TestSample_SumsTaggedStatements(t *testing.T) {
	db := newFakeDB("1.11")
	db.add(1, true, 2)
	db.add(2, true, 3)
	db.add(3, false, 100)
	r := NewSampler(nil).Sample(context.Background(), db, Schema{Database: "app"})
	if !r.StatementsKnown || r.StatementsErr != nil || r.Calls != 5 || r.DBTimeMs != 10 ||
		r.Blocks != 15 || len(r.Statements) != 2 {
		t.Fatalf("reading = %+v", r)
	}
}

// Read is one schema read and one statement sample; a schema failure
// fails it whole.
func TestRead_SchemaFailureIsAnError(t *testing.T) {
	db := newFakeDB("1.11")
	db.schemaErr = errors.New("conn closed")
	if r, err := Read(context.Background(), db); err == nil || !r.At.IsZero() {
		t.Fatalf("Read = %+v, %v, want an error", r, err)
	}
	if db.count("counters") != 0 {
		t.Fatal("statements sampled after the schema read failed")
	}
	db = newFakeDB("1.8")
	db.tables = []fakeTable{{"sage.findings", 100, 1, 1}}
	db.add(1, true, 1)
	r, err := Read(context.Background(), db)
	if err != nil || !r.StatementsKnown || r.SchemaBytes != 100 || r.Calls != 1 {
		t.Fatalf("Read on 1.8 = %+v, %v", r, err)
	}
}
