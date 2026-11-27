package api

import (
	"reflect"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// collection-04: /snapshots/latest serves foreign_keys in one shape, one
// element per constraint, also for a row stored before collection-04
// (one element per column, no schema).
//
// No concurrency test: decoding is a pure function of the stored bytes.

func TestDecodeLatestSnapshot_ForeignKeysLegacyRowIsGrouped(t *testing.T) {
	raw := []byte(`[{"table_name":"orders","referenced_table":"lines",` +
		`"fk_column":"tenant_id","constraint_name":"orders_line_fk"},` +
		`{"table_name":"orders","referenced_table":"lines",` +
		`"fk_column":"order_id","constraint_name":"orders_line_fk"}]`)
	got, err := decodeLatestSnapshot("foreign_keys", raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []collector.ForeignKey{{TableName: "orders", ReferencedTable: "lines",
		ConstraintName: "orders_line_fk", Columns: []string{"tenant_id", "order_id"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded = %#v, want %#v", got, want)
	}
}

func TestDecodeLatestSnapshot_ForeignKeysMalformedIsAnError(t *testing.T) {
	got, err := decodeLatestSnapshot("foreign_keys", []byte(`{"table_name":"t"}`))
	if err == nil || !strings.Contains(err.Error(), "foreign_keys") || got != nil {
		t.Fatalf("decode = %v, %v; want an error naming foreign_keys", got, err)
	}
}

// Other categories are served as stored JSON, as before.
func TestDecodeLatestSnapshot_OtherCategoriesUnchanged(t *testing.T) {
	got, err := decodeLatestSnapshot("system", []byte(`{"total_backends":3}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]any{"total_backends": float64(3)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded = %#v, want %#v", got, want)
	}
}
