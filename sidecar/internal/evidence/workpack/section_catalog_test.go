package workpack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/evidence"
)

// evid-02b without a database: the section builders turn what the reads
// returned into facts with ids, say "unknown" or "absent" where nothing
// was measured, bound what they list, and never let a plan condition or a
// column value into the pack unless the build asked for it.
//
// No concurrent access tests here: the builders are pure functions of
// their input (section_db_test.go builds packs concurrently).

func idx(schema, table, name, method string, keys ...string) indexRow {
	r := indexRow{schema: schema, table: table, name: name, method: method,
		def: "CREATE INDEX " + name, valid: true, keyAtts: len(keys)}
	for _, k := range keys {
		r.keys = append(r.keys, k)
		r.classes = append(r.classes, "3124")
		r.colls = append(r.colls, "0")
		r.opts = append(r.opts, "0")
	}
	return r
}

func TestIndexOverlaps_DuplicateAndLeftPrefix(t *testing.T) {
	a := idx("public", "t", "t_a", "btree", "1")
	ab := idx("public", "t", "t_ab", "btree", "1", "2")
	abDup := idx("public", "t", "t_ab2", "btree", "1", "2")
	otherClass := idx("public", "t", "t_a_pattern", "btree", "1")
	otherClass.classes = []string{"10044"}
	hash := idx("public", "t", "t_a_hash", "hash", "1")
	partial := idx("public", "t", "t_ab_part", "btree", "1", "2")
	partial.pred, partial.partial = "(x > 0)", true
	expr := idx("public", "t", "t_lower", "btree", "0")
	expr.exprs = "lower(name)"
	exprDup := expr
	exprDup.name = "t_lower2"
	otherTable := idx("public", "u", "u_ab", "btree", "1", "2")
	dup, prefix := indexOverlaps([]indexRow{a, ab, abDup, otherClass, hash, partial,
		expr, exprDup, otherTable})
	check := func(what string, got map[string][]string, key, want string) {
		t.Helper()
		if strings.Join(got[key], ",") != want {
			t.Errorf("%s of %s = %v, want [%s]", what, key, got[key], want)
		}
	}
	check("duplicates", dup, "public.t_ab", "public.t_ab2")
	check("duplicates", dup, "public.t_ab2", "public.t_ab")
	check("duplicates", dup, "public.t_lower", "public.t_lower2")
	check("duplicates", dup, "public.t_a", "")
	check("duplicates", dup, "public.t_ab_part", "")
	check("duplicates", dup, "public.u_ab", "")
	check("prefix", prefix, "public.t_a", "public.t_ab,public.t_ab2")
	check("prefix", prefix, "public.t_a_pattern", "")
	check("prefix", prefix, "public.t_a_hash", "")
	check("prefix", prefix, "public.t_lower", "")
	check("prefix", prefix, "public.t_ab", "")
}

func TestBuildIndexes_ScansKnownOnlyWithoutAReset(t *testing.T) {
	rows := []indexRow{idx("public", "t", "t_a", "btree", "1"),
		idx("public", "t", "t_new", "btree", "2")}
	win := relWindow{status: relStatusOK,
		idxFirst: map[string]int64{"public.t_a": 7},
		idxLast:  map[string]int64{"public.t_a": 7, "public.t_new": 3},
		tabFirst: map[string]tableCounters{"public.t": {ins: 10, upd: 1, del: 0}},
		tabLast:  map[string]tableCounters{"public.t": {ins: 25, upd: 4, del: 2}}}
	got := buildIndexes(rows, false, win)
	if got.ScansStatus != relStatusOK || got.Total != 2 || got.Truncated {
		t.Fatalf("indexes %+v", got)
	}
	first := got.Indexes[0]
	if first.ID != "index/idx/public.t_a" || first.Scans != int64(0) {
		t.Fatalf("an index scanned 0 times without a reset: %+v, want scans 0", first)
	}
	w, ok := first.TableWrites.(Writes)
	if !ok || w != (Writes{Inserts: 15, Updates: 3, Deletes: 2}) {
		t.Fatalf("table writes %+v", first.TableWrites)
	}
	if got.Indexes[1].Scans != valueUnknown {
		t.Fatalf("an index missing from the first snapshot: scans %v, want unknown",
			got.Indexes[1].Scans)
	}
	win.status = relStatusReset
	reset := buildIndexes(rows, false, win)
	if reset.ScansStatus != relStatusReset || reset.Indexes[0].Scans != valueUnknown ||
		reset.Indexes[0].TableWrites != valueUnknown {
		t.Fatalf("after a reset %+v, want scans and writes unknown", reset.Indexes[0])
	}
	none := buildIndexes(nil, false, relWindow{status: relStatusUnknown})
	if none.Indexes == nil || len(none.Indexes) != 0 || none.Total != 0 {
		t.Fatalf("no indexes: %+v, want an empty list", none)
	}
}

func TestBuildIndexes_TruncatedPastTheBound(t *testing.T) {
	rows := make([]indexRow, MaxIndexes+1)
	for i := range rows {
		rows[i] = idx("public", "t", fmt.Sprintf("i%05d", i), "btree", "1")
	}
	got := buildIndexes(rows, true, relWindow{status: relStatusUnknown})
	if len(got.Indexes) != MaxIndexes || !got.Truncated || got.Total != MaxIndexes+1 {
		t.Fatalf("%d indexes, truncated %v, total %d", len(got.Indexes), got.Truncated,
			got.Total)
	}
}

func tablesN(n, cols int) []schemaTableRow {
	out := make([]schemaTableRow, n)
	for i := range out {
		out[i] = schemaTableRow{schema: "s", name: fmt.Sprintf("t%05d", i), kind: "table"}
		for c := range cols {
			out[i].columns = append(out[i].columns, ColumnFact{Name: fmt.Sprint("c", c),
				Type: "integer"})
		}
	}
	return out
}

func TestBuildSchema_TableAndColumnBounds(t *testing.T) {
	kept := buildSchema(tablesN(MaxSchemaTables, 1))
	if len(kept.Tables) != MaxSchemaTables || kept.Truncated ||
		kept.TablesTotal != MaxSchemaTables || kept.Tables[0].ID != "schema/table/s.t00000" {
		t.Fatalf("2000 tables: %d kept, truncated %v", len(kept.Tables), kept.Truncated)
	}
	cut := buildSchema(tablesN(MaxSchemaTables+1, 1))
	if len(cut.Tables) != MaxSchemaTables || !cut.Truncated {
		t.Fatalf("2001 tables: %d kept, truncated %v", len(cut.Tables), cut.Truncated)
	}
	wide := buildSchema(tablesN(3, MaxSchemaColumns/2))
	if len(wide.Tables) != 2 || !wide.Truncated || wide.ColumnsTotal != MaxSchemaColumns {
		t.Fatalf("columns bound: %d tables, %d columns, truncated %v", len(wide.Tables),
			wide.ColumnsTotal, wide.Truncated)
	}
	empty := buildSchema(nil)
	if empty.Tables == nil || empty.Truncated {
		t.Fatalf("no tables: %+v", empty)
	}
}

// A table whose constraint or index list the parts bound cut is not kept
// half-listed: it and every table after it are left out, and the section
// says truncated.
func TestBuildSchema_PartsBoundCutsAtTheFirstCutTable(t *testing.T) {
	rows := tablesN(4, 1)
	rows[2].partsCut = true
	s := buildSchema(rows)
	if len(s.Tables) != 2 || !s.Truncated || s.Tables[1].ID != "schema/table/s.t00001" ||
		s.TablesTotal != 4 {
		t.Fatalf("parts cut at table 2: %d kept (%+v), truncated %v, total %d",
			len(s.Tables), s.Tables, s.Truncated, s.TablesTotal)
	}
	whole := buildSchema(tablesN(4, 1))
	if len(whole.Tables) != 4 || whole.Truncated {
		t.Fatalf("no cut: %d kept, truncated %v", len(whole.Tables), whole.Truncated)
	}
}

func TestBuildPlans_ShapeUnreadableAbsentAndConditions(t *testing.T) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	plan := []byte(`[{"Plan": {"Node Type": "Seq Scan", "Relation Name": "orders",
		"Filter": "(email = 'secret@example.com'::text)", "Plan Rows": 5}}]`)
	rows := map[int64]planRow{
		1: {qid: 1, plan: plan, source: "auto_explain", captured: at},
		2: {qid: 2, plan: []byte(`{"not": "a plan"}`), source: "explain", captured: at},
	}
	got := buildPlans([]int64{1, 2, 3}, rows, false)
	if got.Status != "ok" || len(got.Plans) != 3 {
		t.Fatalf("plans %+v", got)
	}
	ok, bad, none := got.Plans[0], got.Plans[1], got.Plans[2]
	if ok.ID != "plan/query/1" || ok.Plan != "ok" || !strings.HasPrefix(ok.Fingerprint, "v2u:") ||
		ok.Shape == nil || ok.Conditions != nil || ok.Source != "auto_explain" {
		t.Fatalf("captured plan %+v", ok)
	}
	if bad.Plan != valueUnreadable || bad.Shape != nil || bad.Fingerprint != "" {
		t.Fatalf("unreadable plan %+v", bad)
	}
	if none.Plan != valueAbsent || none.ID != "plan/query/3" {
		t.Fatalf("plan never captured %+v", none)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "secret@example.com") || strings.Contains(string(raw),
		"Filter") {
		t.Fatalf("plans without conditions hold a condition: %s", raw)
	}
	with := buildPlans([]int64{1}, rows, true)
	raw, _ = json.Marshal(with)
	if !strings.Contains(string(raw), "Filter") || strings.Contains(string(raw), "secret@") {
		t.Fatalf("conditions opted in: %s, want the filter with its literal redacted", raw)
	}
	empty := buildPlans(nil, nil, false)
	if empty.Status != valueAbsent || empty.Plans == nil {
		t.Fatalf("no queryids: %+v, want status absent", empty)
	}
}

func TestBuildConnections_UnknownHoursAndLegacyAges(t *testing.T) {
	aggs := map[int]*connHourAgg{
		9: {snapshots: 2, withAges: 2, states: map[string]float64{"idle": 6, "active": 2},
			ages: map[string]map[string]float64{"idle": {collector.AgeOver1h: 4}}},
		10: {snapshots: 3, withAges: 2, states: map[string]float64{"idle": 3}},
	}
	got := buildConnections(aggs)
	if len(got.Hours) != 168 {
		t.Fatalf("%d hours, want 168", len(got.Hours))
	}
	h9 := got.Hours[9]
	states, ok := h9.States.(map[string]float64)
	if h9.ID != "connections/hour/9" || !ok || states["idle"] != 3 || states["active"] != 1 {
		t.Fatalf("hour 9 %+v", h9)
	}
	ages, ok := h9.Ages.(map[string]map[string]float64)
	if !ok || ages["idle"][collector.AgeOver1h] != 2 {
		t.Fatalf("hour 9 ages %+v", h9.Ages)
	}
	if got.Hours[10].Ages != valueUnknown || got.Hours[10].States == valueUnknown {
		t.Fatalf("hour 10 with a legacy snapshot: %+v, want ages unknown", got.Hours[10])
	}
	if got.Hours[0].States != valueUnknown || got.Hours[0].Ages != valueUnknown ||
		got.Hours[0].Snapshots != 0 {
		t.Fatalf("an hour without snapshots: %+v, want unknown", got.Hours[0])
	}
	if none := buildConnections(nil); len(none.Hours) != 168 ||
		none.Hours[167].States != valueUnknown {
		t.Fatalf("no workload_sessions rows: %+v", none.Hours[167])
	}
}

func TestParseReloptions_KeepsTheUnparsableAsText(t *testing.T) {
	kv, text := parseReloptions([]string{"fillfactor=70",
		"autovacuum_vacuum_scale_factor=0.01", "garbage", "=x"})
	if len(kv) != 2 || kv["fillfactor"] != "70" ||
		kv["autovacuum_vacuum_scale_factor"] != "0.01" {
		t.Fatalf("parsed %v", kv)
	}
	if strings.Join(text, "|") != "garbage|=x" {
		t.Fatalf("text %v", text)
	}
	kv, text = parseReloptions(nil)
	if kv == nil || text == nil || len(kv)+len(text) != 0 {
		t.Fatalf("no reloptions: %v %v, want empty", kv, text)
	}
}

func TestBuildAutovacuum_CountsSeriesAndReset(t *testing.T) {
	last := time.Date(2026, 10, 30, 3, 0, 0, 0, time.UTC)
	win := relWindow{status: relStatusOK,
		tabFirst: map[string]tableCounters{"public.t": {av: 3, aa: 1}},
		tabLast: map[string]tableCounters{"public.t": {av: 5, aa: 4, lastAV: &last,
			reloptions: []string{"fillfactor=70"}}}}
	days := []string{"2026-10-29", "2026-10-30"}
	dead := map[string]map[string]int64{"public.t": {"2026-10-30": 12}}
	got := buildAutovacuum(win, days, dead)
	if len(got.Tables) != 1 {
		t.Fatalf("tables %+v", got)
	}
	f := got.Tables[0]
	if f.ID != "table/av/public.t" || f.Autovacuums != int64(2) || f.Autoanalyzes != int64(3) ||
		f.LastAutovacuum != "2026-10-30T03:00:00Z" || f.LastAutoanalyze != valueAbsent ||
		f.Reloptions["fillfactor"] != "70" {
		t.Fatalf("table %+v", f)
	}
	if len(f.DeadTuples) != 2 || f.DeadTuples[0] != nil || *f.DeadTuples[1] != 12 {
		t.Fatalf("dead tuples %v", f.DeadTuples)
	}
	win.status = relStatusReset
	if r := buildAutovacuum(win, days, dead).Tables[0]; r.Autovacuums != valueUnknown {
		t.Fatalf("after a reset autovacuums %v, want unknown", r.Autovacuums)
	}
}

func TestBuildProvider_UnknownKnownAndUnavailable(t *testing.T) {
	if p := buildProvider("", nil, nil); p.Status != valueUnknown || len(p.Facts) != 1 ||
		p.Facts[0].ID != "provider/status" || p.Facts[0].Value != valueUnknown {
		t.Fatalf("no resource: %+v", p)
	}
	if p := buildProvider("arn:x", nil, nil); p.Status != valueUnknown {
		t.Fatalf("no telemetry row: %+v", p)
	}
	row := &hostRow{class: "db.r6g.large", vcpus: 2, mem: 16 << 30, storage: 100 << 30}
	p := buildProvider("arn:x", row, nil)
	byID := map[string]any{}
	for _, f := range p.Facts {
		byID[f.ID] = f.Value
	}
	if p.Status != "ok" || byID["provider/instance_class"] != "db.r6g.large" ||
		byID["provider/vcpus"] != 2 || byID["provider/serverless"] != false ||
		byID["provider/memory_bytes"] != float64(16<<30) {
		t.Fatalf("known provider %+v", p)
	}
	pgErr := &pgconn.PgError{Code: "42P01", Message: "relation does not exist"}
	u := buildProvider("arn:x", nil, fmt.Errorf("read: %w", pgErr))
	if u.Status != valueUnavailable || u.ErrorClass != "sqlstate 42P01" ||
		u.Facts[0].Value != valueUnavailable {
		t.Fatalf("failed read: %+v", u)
	}
	if c := errorClass(context.DeadlineExceeded); c != "timeout" {
		t.Fatalf("deadline class %q", c)
	}
	if c := errorClass(errors.New("boom")); c != "error" {
		t.Fatalf("plain class %q", c)
	}
}

func TestBuildStats_UnreadableTablesAndSamples(t *testing.T) {
	mcv := "{marker}"
	rows := []statRow{{table: "public.t", column: "a", nullFrac: 0.5, nDistinct: -1,
		width: 4, mcv: &mcv}}
	got := buildStats([]string{"public.t", "public.secret"}, rows, false)
	if len(got.Tables) != 2 || got.Tables[1].Status != "ok" ||
		got.Tables[0].ID != "stats/table/public.secret" || got.Tables[0].Status != valueUnknown {
		t.Fatalf("tables %+v", got.Tables)
	}
	if len(got.Columns) != 1 || got.Columns[0].ID != "stats/column/public.t.a" ||
		got.Columns[0].NullFrac != 0.5 || got.Columns[0].MostCommonVals != nil {
		t.Fatalf("columns %+v (value samples off)", got.Columns)
	}
	with := buildStats([]string{"public.t"}, rows, true)
	if !with.ValueSamples || with.Columns[0].MostCommonVals == nil {
		t.Fatalf("value samples on: %+v", with.Columns)
	}
	if none := buildStats(nil, nil, false); none.Tables == nil || none.Columns == nil {
		t.Fatalf("nothing referenced: %+v", none)
	}
}

func TestBuildContext_BriefAbsentAndFacts(t *testing.T) {
	at := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	none := buildContext(nil)
	brief, ok := none.Brief.(map[string]string)
	if !ok || brief["id"] != "context/brief" || brief["brief"] != valueAbsent ||
		none.Facts == nil {
		t.Fatalf("no brief: %+v", none)
	}
	got := buildContext([]factRow{
		{typ: "owner_brief", kind: "database", subject: "*",
			value: map[string]string{"owner": "payments"}, by: "ops@example.com", at: at},
		{typ: "append_only", kind: "table", subject: "public.events", by: "dba", at: at},
	})
	b, ok := got.Brief.(ContextFact)
	if !ok || b.ID != "context/brief" || b.Value["owner"] != "payments" ||
		b.ConfirmedBy != "ops@example.com" || b.ConfirmedAt != "2026-11-02T08:00:00Z" {
		t.Fatalf("brief %+v", got.Brief)
	}
	if len(got.Facts) != 1 || got.Facts[0].ID != "context/fact/append_only.table.public.events" {
		t.Fatalf("facts %+v", got.Facts)
	}
}

func TestResolve_TwoPartIDsOfTheNewSections(t *testing.T) {
	w := fourWeeks()
	h := synthHistory(w)
	p := mustAssemble(t, synthRequest(w), h)
	for _, id := range []string{"provider/status", "context/brief", "connections/hour/0",
		"connections/hour/167"} {
		if _, err := Resolve(p, id); err != nil {
			t.Errorf("Resolve(%q): %v", id, err)
		}
	}
	for _, id := range []string{"provider/", "context/", "context//brief",
		"connections/hour/168", "setting/../x", "provider"} {
		if _, err := Resolve(p, id); !errors.Is(err, ErrUnknownEvidence) {
			t.Errorf("Resolve(%q): %v, want ErrUnknownEvidence", id, err)
		}
	}
	for _, name := range []string{fileIndexes, fileSettings, fileAutovacuum, fileSchema,
		filePlans, fileConnections, fileProvider, fileStats, fileContext} {
		if p.File(name) == nil {
			t.Errorf("section %s is missing from a pack with no catalog data", name)
		}
	}
	if p.Manifest.Completeness == nil || p.Manifest.Completeness.KnownShare >= 1 {
		t.Fatalf("completeness %+v, want a share below 1 with no catalog data",
			p.Manifest.Completeness)
	}
}

func TestAssemble_ABriefChangesTheHash(t *testing.T) {
	w := fourWeeks()
	h := synthHistory(w)
	before := mustAssemble(t, synthRequest(w), h)
	h.cat.facts = []factRow{{typ: "owner_brief", kind: "database", subject: "*",
		value: map[string]string{"owner": "x"}, by: "a", at: w.From}}
	with := mustAssemble(t, synthRequest(w), h)
	h.cat.facts = nil
	after := mustAssemble(t, synthRequest(w), h)
	if with.ContentSHA256 == before.ContentSHA256 || after.ContentSHA256 != before.ContentSHA256 {
		t.Fatalf("hashes before %s with %s after %s", before.ContentSHA256,
			with.ContentSHA256, after.ContentSHA256)
	}
	if _, err := Resolve(with, "context/brief"); err != nil {
		t.Fatal(err)
	}
}

func TestDecode_APackWithoutTheNewSectionsStillOpens(t *testing.T) {
	w := fourWeeks()
	p := mustAssemble(t, synthRequest(w), synthHistory(w))
	var legacy []evidence.File
	for _, f := range p.Files {
		switch f.Name {
		case fileHeader, fileCoverage, fileWorkload, fileGrowth:
			legacy = append(legacy, f)
		}
	}
	old, err := seal(synthRequest(w), w, legacy)
	if err != nil || len(old.Files) != 4 {
		t.Fatalf("seal a pack of the evid-02a sections: %v", err)
	}
	back, err := Decode(old.Body, old.Manifest)
	if err != nil || back.ContentSHA256 != old.ContentSHA256 {
		t.Fatalf("decode a pre-evid-02b pack: %v", err)
	}
	if _, err := Resolve(back, "coverage/window/all"); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(back, "provider/status"); !errors.Is(err, ErrUnknownEvidence) {
		t.Fatalf("a pre-evid-02b pack resolves provider/status: %v", err)
	}
}
