package provideradvice

import (
	"fmt"
	"strings"
	"testing"
)

// fakeAdvisorView stands in for the google_db_advisor extension's view:
// the documented (index, estimated_storage_size_in_mb) shape.
const fakeAdvisorView = `CREATE VIEW public.google_db_advisor_recommended_indexes AS
SELECT * FROM (VALUES
  ('CREATE INDEX ON "public"."users"("email")'::text, 8::bigint),
  ('CREATE INDEX ON "public"."orders"("customer_id", "created_at")', 120)
) AS v("index", estimated_storage_size_in_mb)`

func TestCloudSQL_AbsentViewIsNoAdviceAndNoError(t *testing.T) {
	pool, ctx := testPool(t)
	src, err := NewCloudSQLSource("cloud-sql", pool)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := src.Fetch(ctx)
	if err != nil || len(recs) != 0 {
		t.Fatalf("recs %v, err %v", recs, err)
	}
}

func TestCloudSQL_ReadsTheAdvisorView(t *testing.T) {
	pool, ctx := testPool(t)
	mustExec(t, pool, fakeAdvisorView)
	src, err := NewCloudSQLSource("alloydb", pool)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := src.Fetch(ctx)
	if err != nil || len(recs) != 2 || src.Provider() != "alloydb" {
		t.Fatalf("recs %+v, err %v", recs, err)
	}
	first := recs[0]
	if first.Kind != KindCreateIndex || first.Provider != "alloydb" ||
		first.DDL != `CREATE INDEX ON "public"."users"("email")` ||
		first.Object != "public.users" || !strings.Contains(first.Reason, "8 MB") ||
		first.ObservedAt.IsZero() || !strings.HasPrefix(first.ProviderID, "index:") {
		t.Fatalf("first = %+v", first)
	}
	again, err := src.Fetch(ctx)
	if err != nil || again[0].ProviderID != first.ProviderID ||
		again[1].ProviderID == first.ProviderID {
		t.Fatalf("provider ids must be stable and distinct: %v / %v (%v)", first.ProviderID,
			again, err)
	}
}

func TestCloudSQL_FindsTheViewInAnotherSchema(t *testing.T) {
	pool, ctx := testPool(t)
	mustExec(t, pool, "CREATE SCHEMA pa_test")
	mustExec(t, pool, strings.Replace(fakeAdvisorView, "public.google", "pa_test.google", 1))
	src, _ := NewCloudSQLSource("cloud-sql", pool)
	recs, err := src.Fetch(ctx)
	if err != nil || len(recs) != 2 {
		t.Fatalf("recs %+v, err %v", recs, err)
	}
}

func TestCloudSQL_UnknownShapeIsUnknownNotParsed(t *testing.T) {
	pool, ctx := testPool(t)
	mustExec(t, pool, `CREATE VIEW public.google_db_advisor_recommended_indexes AS
		SELECT 'CREATE INDEX ON t (a)'::text AS recommendation`)
	src, _ := NewCloudSQLSource("cloud-sql", pool)
	recs, err := src.Fetch(ctx)
	if err != nil || len(recs) != 1 {
		t.Fatalf("recs %+v, err %v", recs, err)
	}
	if recs[0].Kind != KindUnknown || recs[0].DDL != "" ||
		!strings.Contains(recs[0].Reason, "recommendation") {
		t.Fatalf("rec = %+v", recs[0])
	}
}

func TestCloudSQL_ReadIsBounded(t *testing.T) {
	pool, ctx := testPool(t)
	mustExec(t, pool, fmt.Sprintf(`CREATE VIEW public.google_db_advisor_recommended_indexes AS
		SELECT format('CREATE INDEX ON public.t%%s (a)', g) AS "index",
		       1::bigint AS estimated_storage_size_in_mb
		  FROM generate_series(1, %d) g`, maxCloudSQLRows+50))
	src, _ := NewCloudSQLSource("cloud-sql", pool)
	recs, err := src.Fetch(ctx)
	if err != nil || len(recs) != maxCloudSQLRows {
		t.Fatalf("got %d recs (err %v), want the cap %d", len(recs), err, maxCloudSQLRows)
	}
}

func TestCloudSQL_ViewErrorPropagates(t *testing.T) {
	pool, ctx := testPool(t)
	mustExec(t, pool, `CREATE VIEW public.google_db_advisor_recommended_indexes AS
		SELECT (1/0)::text AS "index", 1::bigint AS estimated_storage_size_in_mb`)
	src, _ := NewCloudSQLSource("cloud-sql", pool)
	recs, err := src.Fetch(ctx)
	if err == nil || recs != nil || !strings.Contains(err.Error(), "advisor view") {
		t.Fatalf("recs %v, err %v", recs, err)
	}
}

func TestNewCloudSQLSource_RejectsBadInput(t *testing.T) {
	if _, err := NewCloudSQLSource("cloud-sql", nil); err == nil {
		t.Fatal("nil pool accepted")
	}
	pool, _ := testPool(t)
	if _, err := NewCloudSQLSource("rds", pool); err == nil {
		t.Fatal("a provider without the advisor view was accepted")
	}
}
