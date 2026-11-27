package tuning

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/explain"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// security-10: a rehearsal statement that fails on the clone with a data
// exception keeps its SQLSTATE but not the server text, which can quote
// the clone's data; any other failure keeps its text.
// No concurrent access tests: one measurer owns its clone connection.

func cloneMeasurerOn(t *testing.T) CloneMeasurer {
	t.Helper()
	_, pinned := testdb.AppPathDatabase(t, "tuning_safe_error")
	conn, err := pinned.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Release)
	m, err := NewCloneMeasurer(context.Background(), conn.Conn(), time.Second, nil)
	if err != nil {
		t.Fatalf("NewCloneMeasurer: %v", err)
	}
	return m
}

func TestCloneMeasure_DataExceptionTextIsWithheld(t *testing.T) {
	m := cloneMeasurerOn(t)
	_, err := m.Measure(context.Background(), "SELECT 'secret-value-7'::int FROM orders")
	if err == nil {
		t.Fatal("a cast of a non-number measured without an error")
	}
	if strings.Contains(err.Error(), "secret-value-7") ||
		!strings.Contains(err.Error(), explain.WithheldDataMessage) {
		t.Fatalf("err = %q, want the data exception withheld", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "22P02" {
		t.Fatalf("err = %v, want SQLSTATE 22P02 kept", err)
	}
	qs := m.m.before(context.Background(), []RehearsalStatement{{QueryID: 7,
		Text: "SELECT 'secret-value-7'::int FROM orders"}})
	if len(qs) != 1 || qs[0].Error == "" || strings.Contains(qs[0].Error, "secret-value-7") {
		t.Fatalf("rehearsed = %+v, want an error without the value", qs)
	}
}

func TestCloneMeasure_OtherErrorsKeepTheirText(t *testing.T) {
	m := cloneMeasurerOn(t)
	_, err := m.Measure(context.Background(), "SELECT * FROM missing_orders")
	if err == nil || !strings.Contains(err.Error(), `relation "missing_orders" does not exist`) {
		t.Fatalf("err = %v, want the missing relation named", err)
	}
	got, err := m.Measure(context.Background(), "SELECT app_only FROM orders")
	if err != nil || got.Cost <= 0 {
		t.Fatalf("measure = %+v, %v, want a plan cost and no error", got, err)
	}
}
