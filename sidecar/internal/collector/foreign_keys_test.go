package collector

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// collection-04: DecodeForeignKeys reads a stored foreign_keys document
// of either shape. Rows written before collection-04 hold one element per
// column and no schema; they decode to one constraint per (table,
// constraint) with an empty Schema (unknown), columns in row order.
//
// No concurrency test: decoding is a pure function of its input.

func TestDecodeForeignKeys_LegacyPerColumnRowsAreGrouped(t *testing.T) {
	raw := []byte(`[
	 {"table_name":"orders","referenced_table":"lines","fk_column":"tenant_id",
	  "constraint_name":"orders_line_fk"},
	 {"table_name":"orders","referenced_table":"customers","fk_column":"customer_id",
	  "constraint_name":"orders_customer_fk"},
	 {"table_name":"orders","referenced_table":"lines","fk_column":"order_id",
	  "constraint_name":"orders_line_fk"},
	 {"table_name":"invoices","referenced_table":"lines","fk_column":"line_id",
	  "constraint_name":"orders_line_fk"}]`)
	got, err := DecodeForeignKeys(raw)
	if err != nil {
		t.Fatalf("DecodeForeignKeys: %v", err)
	}
	want := []ForeignKey{
		{TableName: "orders", ReferencedTable: "lines", ConstraintName: "orders_line_fk",
			Columns: []string{"tenant_id", "order_id"}},
		{TableName: "orders", ReferencedTable: "customers",
			ConstraintName: "orders_customer_fk", Columns: []string{"customer_id"}},
		{TableName: "invoices", ReferencedTable: "lines", ConstraintName: "orders_line_fk",
			Columns: []string{"line_id"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded = %+v\nwant      %+v", got, want)
	}
}

// Happy path: a document in the current shape decodes to what was written.
func TestDecodeForeignKeys_CurrentShapeRoundTrips(t *testing.T) {
	in := []ForeignKey{{Schema: "tenant_a", TableName: "orders", RefSchema: "core",
		ReferencedTable: "customers", ConstraintName: "orders_customer_fk",
		Columns: []string{"b", "a"}, OID: 16_402}}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{`"schemaname":"tenant_a"`, `"ref_schemaname":"core"`,
		`"columns":["b","a"]`, `"oid":16402`} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("stored element %s lacks %s", raw, field)
		}
	}
	got, err := DecodeForeignKeys(raw)
	if err != nil {
		t.Fatalf("DecodeForeignKeys: %v", err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("decoded = %+v, want %+v", got, in)
	}
}

// Nil/empty: null and [] are no foreign keys, not an error.
func TestDecodeForeignKeys_NullAndEmpty(t *testing.T) {
	for _, doc := range []string{`null`, `[]`, ` [ ] `} {
		got, err := DecodeForeignKeys([]byte(doc))
		if err != nil || len(got) != 0 {
			t.Errorf("%q: decoded %+v, %v; want none", doc, got, err)
		}
	}
}

// Invalid input: a document that is not a list of objects is an error
// naming the category, never a partial result.
func TestDecodeForeignKeys_MalformedIsAnError(t *testing.T) {
	for _, doc := range []string{``, `{"table_name":"t"}`, `[1]`, `[{"columns":"a"}]`,
		`[{"table_name":`} {
		got, err := DecodeForeignKeys([]byte(doc))
		if err == nil || !strings.Contains(err.Error(), "foreign_keys") {
			t.Errorf("%q: err = %v, want an error naming foreign_keys", doc, err)
		}
		if got != nil {
			t.Errorf("%q: decoded %+v alongside the error, want nil", doc, got)
		}
	}
}

// Invalid input: a legacy document from a schema-per-tenant database
// holds the same (table, constraint, column) once per tenant. The
// tenants cannot be told apart, so they decode to one key whose columns
// are not repeated.
func TestDecodeForeignKeys_LegacyTenantDuplicatesAreNotRepeated(t *testing.T) {
	row := `{"table_name":"orders","referenced_table":"customers",` +
		`"fk_column":"customer_id","constraint_name":"orders_customer_fk"}`
	got, err := DecodeForeignKeys([]byte("[" + row + "," + row + "]"))
	if err != nil {
		t.Fatalf("DecodeForeignKeys: %v", err)
	}
	want := []ForeignKey{{TableName: "orders", ReferencedTable: "customers",
		ConstraintName: "orders_customer_fk", Columns: []string{"customer_id"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded = %+v, want %+v", got, want)
	}
}
