package optimizer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeExtensions is the extension-source seam: it answers from a fixed
// installed set (or fails with err) and records every lookup.
type fakeExtensions struct {
	installed map[string]bool
	err       error
	mu        sync.Mutex
	asked     [][]string
}

func (f *fakeExtensions) source(_ context.Context, names []string) (map[string]bool, error) {
	f.mu.Lock()
	f.asked = append(f.asked, slices.Clone(names))
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]bool{}
	for _, n := range names {
		if f.installed[n] {
			out[n] = true
		}
	}
	return out, nil
}

func (f *fakeExtensions) lookups() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}

// logLines collects what the validator logs.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) log(level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(format, args...))
}

func (l *logLines) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func extValidator(installed ...string) (*Validator, *fakeExtensions, *logLines) {
	fe := &fakeExtensions{installed: map[string]bool{}}
	for _, n := range installed {
		fe.installed[n] = true
	}
	logs := &logLines{}
	v := NewValidator(nil, fnTestOptimizerConfig(), logs.log)
	v.extensionSource = fe.source
	return v, fe, logs
}

// extContext is a table with the columns the extension tests index.
func extContext() TableContext {
	return TableContext{Schema: "public", Table: "slots", Columns: []ColumnInfo{
		{Name: "id", Type: "integer"},
		{Name: "name", Type: "text"},
		{Name: "during", Type: "tsrange"},
		{Name: "payload", Type: "jsonb"},
		{Name: "geom", Type: "geometry"},
		{Name: "area", Type: "geography(Polygon,4326)"},
		{Name: "emb", Type: "vector"},
		{Name: "customer_id", Type: "integer"},
	}}
}

func extRec(method, keys string) Recommendation {
	return Recommendation{Table: "public.slots", IndexType: method,
		DDL: "CREATE INDEX CONCURRENTLY idx_slots_probe ON public.slots USING " +
			method + " " + keys}
}

func wantExtAdmitted(t *testing.T, ok bool, reason string) {
	t.Helper()
	if !ok {
		t.Fatalf("rejected: %s", reason)
	}
	if reason != "" {
		t.Errorf("admitted with reason %q, want none", reason)
	}
}

func wantExtRefused(t *testing.T, ok bool, reason string, want []string, not ...string) {
	t.Helper()
	if ok {
		t.Fatal("admitted, want refused")
	}
	for _, w := range want {
		if !strings.Contains(reason, w) {
			t.Errorf("reason %q does not contain %q", reason, w)
		}
	}
	for _, n := range not {
		if strings.Contains(reason, n) {
			t.Errorf("reason %q names %q", reason, n)
		}
	}
}

// Happy path (fails today: "st_" in gist_trgm_ops reads as PostGIS): the
// canonical trigram GiST index is admitted with pg_trgm and no PostGIS,
// and only pg_trgm is looked up.
func TestExtension_GistTrigramWithPgTrgmAdmitted(t *testing.T) {
	v, fe, _ := extValidator("pg_trgm")
	ok, reason := v.Validate(context.Background(),
		extRec("gist", "(name gist_trgm_ops)"), extContext())
	wantExtAdmitted(t, ok, reason)
	if got := fe.lookups(); len(got) != 1 || !slices.Equal(got[0], []string{"pg_trgm"}) {
		t.Errorf("lookups = %v, want [[pg_trgm]]", got)
	}
}

// Invalid input (today the reason says postgis): without pg_trgm the
// trigram GiST index is refused naming pg_trgm and how to install it.
func TestExtension_GistTrigramWithoutPgTrgmNamesPgTrgm(t *testing.T) {
	v, _, _ := extValidator("postgis")
	ok, reason := v.Validate(context.Background(),
		extRec("gist", "(name gist_trgm_ops)"), extContext())
	wantExtRefused(t, ok, reason,
		[]string{"pg_trgm", "not installed", "CREATE EXTENSION pg_trgm"}, "postgis")
}

// Boundary (fails today: "st_" in idx_slots_last_seen): a GiST index on a
// range column needs no extension, whatever the index is called.
func TestExtension_IndexNameWithStPrefixNeedsNothing(t *testing.T) {
	v, fe, _ := extValidator()
	rec := Recommendation{Table: "public.slots", IndexType: "gist",
		DDL: "CREATE INDEX CONCURRENTLY idx_slots_last_seen ON public.slots USING gist (during)"}
	ok, reason := v.Validate(context.Background(), rec, extContext())
	wantExtAdmitted(t, ok, reason)
	if got := fe.lookups(); len(got) != 0 {
		t.Errorf("lookups = %v, want none (nothing required)", got)
	}
}

// Boundary (fails today: "trgm" anywhere, the index name included): a GIN
// index on jsonb named idx_trgm_cache needs no extension.
func TestExtension_GinNamedTrgmOnJsonbNeedsNothing(t *testing.T) {
	v, fe, _ := extValidator()
	rec := Recommendation{Table: "public.slots", IndexType: "gin",
		DDL: "CREATE INDEX CONCURRENTLY idx_trgm_cache ON public.slots USING gin (payload)"}
	ok, reason := v.Validate(context.Background(), rec, extContext())
	wantExtAdmitted(t, ok, reason)
	if got := fe.lookups(); len(got) != 0 {
		t.Errorf("lookups = %v, want none (nothing required)", got)
	}
}

// A GiST key on a column whose type is geometry or geography requires
// PostGIS even with the default operator class.
func TestExtension_GeometryColumnRequiresPostgis(t *testing.T) {
	for _, keys := range []string{"(geom)", "(area)", "(GEOM)"} {
		v, _, _ := extValidator("pg_trgm")
		ok, reason := v.Validate(context.Background(), extRec("gist", keys), extContext())
		wantExtRefused(t, ok, reason,
			[]string{"postgis", "CREATE EXTENSION postgis"}, "pg_trgm")
	}
}

// Nil/empty: with no column types (tc.Columns == nil) the PostGIS
// operator class alone still requires postgis.
func TestExtension_PostgisOpclassWithoutColumns(t *testing.T) {
	for _, keys := range []string{"(geom gist_geometry_ops_nd)",
		"(geom gist_geometry_ops_2d)", "(area gist_geography_ops)"} {
		v, fe, _ := extValidator()
		ok, reason := v.checkExtensionRequired(context.Background(),
			extRec("gist", keys), TableContext{})
		wantExtRefused(t, ok, reason, []string{"postgis"})
		if got := fe.lookups(); len(got) != 1 || !slices.Equal(got[0], []string{"postgis"}) {
			t.Errorf("%s: lookups = %v, want [[postgis]]", keys, got)
		}
	}
}

// Error propagation: a failed extension read admits the candidate (PostgreSQL
// or HypoPG validates the DDL) and logs what was attempted and why it failed.
func TestExtension_ReadErrorPassesAndLogs(t *testing.T) {
	v, fe, logs := extValidator()
	fe.err = errors.New("connect: connection refused")
	ok, reason := v.Validate(context.Background(),
		extRec("gist", "(name gist_trgm_ops)"), extContext())
	wantExtAdmitted(t, ok, reason)
	got := logs.joined()
	for _, want := range []string{"pg_trgm", "public.slots", "connection refused"} {
		if !strings.Contains(got, want) {
			t.Errorf("log %q does not contain %q", got, want)
		}
	}
}

// Error propagation through the real read path: a catalog connection that
// cannot begin a transaction is one attempted read, logged, and admitted.
func TestExtension_CatalogBeginErrorPassesAndLogs(t *testing.T) {
	fc := &failingCatalog{err: errors.New("pool closed")}
	logs := &logLines{}
	v := NewValidator(nil, fnTestOptimizerConfig(), logs.log)
	v.catalog = fc
	ok, reason := v.checkExtensionRequired(context.Background(),
		extRec("gist", "(id gist_int4_ops, name gist_trgm_ops)"), extContext())
	wantExtAdmitted(t, ok, reason)
	if n := fc.begun.Load(); n != 1 {
		t.Errorf("reads = %d, want 1 (one read per Validate)", n)
	}
	if got := logs.joined(); !strings.Contains(got, "pool closed") ||
		!strings.Contains(got, "btree_gist") {
		t.Errorf("log %q does not name the extensions and the cause", got)
	}
}

// Every extension one index needs is read once, and every missing one is
// named in a stable order.
func TestExtension_SeveralRequiredOneReadAllNamed(t *testing.T) {
	v, fe, _ := extValidator()
	ok, reason := v.Validate(context.Background(),
		extRec("gist", "(id gist_int4_ops, name gist_trgm_ops)"), extContext())
	wantExtRefused(t, ok, reason, []string{"btree_gist", "pg_trgm"})
	if strings.Index(reason, "btree_gist") > strings.Index(reason, "pg_trgm") {
		t.Errorf("reason %q is not in sorted extension order", reason)
	}
	got := fe.lookups()
	if len(got) != 1 || !slices.Equal(got[0], []string{"btree_gist", "pg_trgm"}) {
		t.Errorf("lookups = %v, want one read of [btree_gist pg_trgm]", got)
	}
	v, _, _ = extValidator("btree_gist")
	ok, reason = v.Validate(context.Background(),
		extRec("gist", "(id gist_int4_ops, name gist_trgm_ops)"), extContext())
	wantExtRefused(t, ok, reason, []string{"pg_trgm"}, "btree_gist")
}

// Operator classes map to the extension that ships them; core operator
// classes, a btree's int4_ops and unknown names need nothing.
func TestRequiredExtensions_OpclassMap(t *testing.T) {
	cases := []struct {
		method, keys string
		want         []string
	}{
		{"gin", "(name gin_trgm_ops)", []string{"pg_trgm"}},
		{"gist", "(name public.gist_trgm_ops)", []string{"pg_trgm"}},
		{"gist", "(name GIST_TRGM_OPS)", []string{"pg_trgm"}},
		{"gist", "(id gist_int8_ops)", []string{"btree_gist"}},
		{"gist", "(name gist_text_ops)", []string{"btree_gist"}},
		{"gin", "(customer_id int4_ops)", []string{"btree_gin"}},
		{"gin", "(name text_ops)", []string{"btree_gin"}},
		{"btree", "(customer_id int4_ops)", nil},
		{"btree", "(name text_pattern_ops)", nil},
		{"hnsw", "(emb vector_cosine_ops)", []string{"vector"}},
		{"ivfflat", "(emb vector_l2_ops) WITH (lists = 100)", []string{"vector"}},
		{"hnsw", "(emb halfvec_ip_ops)", []string{"vector"}},
		{"hnsw", "(emb sparsevec_l2_ops)", []string{"vector"}},
		{"gist", "(geom gist_geometry_ops_nd)", []string{"postgis"}},
		{"spgist", "(geom spgist_geometry_ops_2d)", []string{"postgis"}},
		{"brin", "(geom brin_geometry_inclusion_ops_2d)", []string{"postgis"}},
		{"hnsw", "(emb bit_hamming_ops)", []string{"vector"}},
		{"gin", "(payload gin_hstore_ops)", []string{"hstore"}},
		{"gin", "(id gin__int_ops)", []string{"intarray"}},
		{"gist", "(name gist_ltree_ops)", []string{"ltree"}},
		{"gist", "(name gist_cube_ops)", []string{"cube"}},
		{"gin", "(payload jsonb_path_ops)", nil},
		{"gin", "(payload vectorize_ops)", nil},
		{"gist", "(during range_ops)", nil},
		{"gist", `(name "gist_trgm_ops_custom")`, nil},
		{"gist", "(id gist_int4_ops, geom)", []string{"btree_gist", "postgis"}},
	}
	for _, c := range cases {
		got := requiredExtensions(extRec(c.method, c.keys), extContext())
		if !slices.Equal(got, c.want) {
			t.Errorf("%s %s: required = %v, want %v", c.method, c.keys, got, c.want)
		}
	}
}

// Expression keys require PostGIS only through a geometry column they
// reference; a function name starting with st_ is not evidence.
func TestRequiredExtensions_ExpressionKeys(t *testing.T) {
	tc := extContext()
	if got := requiredExtensions(extRec("gist", "((st_transform(geom, 3857)))"), tc); !slices.Equal(
		got, []string{"postgis"}) {
		t.Errorf("expression on geom: required = %v, want [postgis]", got)
	}
	if got := requiredExtensions(extRec("btree", "((lower(name)))"), tc); got != nil {
		t.Errorf("expression on text: required = %v, want none", got)
	}
}

// Nil/empty/invalid input: no DDL, an unparseable DDL and a table context
// without columns require nothing and read nothing.
func TestExtension_EmptyAndUnparseableNeedNothing(t *testing.T) {
	for _, ddl := range []string{"", "not an index", "CREATE INDEX CONCURRENTLY i ON t USING gist ("} {
		v, fe, _ := extValidator()
		ok, reason := v.checkExtensionRequired(context.Background(),
			Recommendation{DDL: ddl, IndexType: "gist"}, TableContext{})
		wantExtAdmitted(t, ok, reason)
		if got := fe.lookups(); len(got) != 0 {
			t.Errorf("%q: lookups = %v, want none", ddl, got)
		}
	}
}

// Nil: with no database and no source the check cannot read and admits.
func TestExtension_NoDatabaseAdmits(t *testing.T) {
	logs := &logLines{}
	v := NewValidator(nil, fnTestOptimizerConfig(), logs.log)
	ok, reason := v.checkExtensionRequired(context.Background(),
		extRec("gist", "(name gist_trgm_ops)"), extContext())
	wantExtAdmitted(t, ok, reason)
}

// The other validator checks keep their order: a candidate that fails
// both an earlier check and the extension check reports the earlier one,
// and BRIN correlation (a later check) is not reached.
func TestExtension_ValidatorOrderUnchanged(t *testing.T) {
	v, _, _ := extValidator()
	rec := extRec("gist", "(name gist_trgm_ops)")
	rec.DDL = strings.Replace(rec.DDL, " CONCURRENTLY", "", 1)
	ok, reason := v.Validate(context.Background(), rec, extContext())
	wantExtRefused(t, ok, reason, []string{"CONCURRENTLY"}, "pg_trgm")

	tc := extContext()
	tc.IndexCount = 10
	ok, reason = v.Validate(context.Background(), extRec("gist", "(name gist_trgm_ops)"), tc)
	wantExtRefused(t, ok, reason, []string{"maximum indexes"}, "pg_trgm")

	tc = extContext()
	tc.ColStats = []ColStat{{Column: "geom", Correlation: 0.1}}
	ok, reason = v.Validate(context.Background(),
		extRec("brin", "(geom brin_geometry_inclusion_ops_2d)"), tc)
	wantExtRefused(t, ok, reason, []string{"postgis"}, "correlation")
}

// Concurrent access: one Validator checks many candidates in parallel and
// each gets its own verdict.
func TestExtension_ConcurrentValidate(t *testing.T) {
	v, fe, _ := extValidator("pg_trgm")
	var refused atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			keys := "(name gist_trgm_ops)"
			if i%2 == 1 {
				keys = "(id gist_int4_ops)"
			}
			if ok, _ := v.Validate(context.Background(), extRec("gist", keys),
				extContext()); !ok {
				refused.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if got := refused.Load(); got != 8 {
		t.Errorf("refused = %d, want 8 (the btree_gist half)", got)
	}
	if got := len(fe.lookups()); got != 16 {
		t.Errorf("lookups = %d, want 16 (one per Validate)", got)
	}
}

// No state transitions: the extension check is a pure function of the
// candidate, the table context and one catalog read.
