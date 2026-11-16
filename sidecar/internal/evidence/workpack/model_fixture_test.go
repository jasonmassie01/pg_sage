package workpack

import (
	"bytes"
	"testing"

	"github.com/pg-sage/sidecar/internal/evidence"
)

// The evid-02c model-tier fixture: the synthetic four weeks with a secret
// in a statement text, a check constraint, an index predicate, a plan
// condition, a column's most common values, the owner brief and a
// replication setting. None of seededSecrets may reach the model tier.

var seededSecrets = []string{
	"sk-test-4f9a8b7c6d5e3f21",
	"4111111111111111",
	"sk-test-constraint-77aa",
	"secret-state-42",
	"sk-test-planfilter-99aa",
	"mcv-marker-7319",
	"other-common-value",
	"zzzz-bound",
	"quarter-secret-note",
	"hunter2-conninfo",
	"owner@example.com",
}

const fixtureIndexDef = "CREATE INDEX o_live ON public.orders USING btree (id) " +
	"WHERE (state = 'secret-state-42'::text)"

func strPtr(s string) *string { return &s }

// withSections is p with the named section files replaced by v's
// canonical JSON, resealed.
func withSections(t *testing.T, p *Pack, replace map[string]any) *Pack {
	t.Helper()
	files := make([]evidence.File, 0, len(p.Files))
	for _, f := range p.Files {
		if v, ok := replace[f.Name]; ok {
			data, err := canonical(v)
			if err != nil {
				t.Fatalf("encode %s: %v", f.Name, err)
			}
			f = evidence.File{Name: f.Name, Data: data}
		}
		files = append(files, f)
	}
	out, err := seal(synthRequest(p.Window), p.Window, files)
	if err != nil {
		t.Fatalf("seal the fixture: %v", err)
	}
	return out
}

func fixtureSchema() Schema {
	return Schema{Tables: []TableFact{{ID: "schema/table/public.orders",
		Table: "public.orders", Kind: "table", SizeBytes: 8192,
		Columns: []ColumnFact{{Name: "id", Type: "bigint", NotNull: true},
			{Name: "token", Type: "text"}, {Name: "state", Type: "text"}},
		Constraints: []string{"CHECK ((token <> 'sk-test-constraint-77aa'::text))"},
		Indexes:     []string{fixtureIndexDef}}}, TablesTotal: 1, ColumnsTotal: 3}
}

func fixtureCatalog() map[string]any {
	return map[string]any{
		fileSchema: fixtureSchema(),
		fileIndexes: Indexes{Indexes: []IndexFact{{ID: "index/idx/public.orders.o_live",
			Schema: "public", Table: "orders", Index: "o_live", Definition: fixtureIndexDef,
			Method: "btree", SizeBytes: 16384, Valid: true, Partial: true,
			Scans: valueUnknown, TableWrites: valueUnknown, DuplicateOf: []string{},
			PrefixOf: []string{}}}, Total: 1, ScansStatus: valueUnknown},
		filePlans: Plans{Status: relStatusOK, Plans: []PlanFact{{ID: "plan/query/1",
			QueryID: "1", Plan: relStatusOK, Fingerprint: "v2u:abc",
			Shape: map[string]any{"Node Type": "Index Scan", "Schema": "public",
				"Relation Name": "orders", "Index Name": "o_live", "Plan Rows": 3},
			Conditions: map[string]any{"Node Type": "Index Scan",
				"Index Cond": "(token = 'sk-test-planfilter-99aa'::text)"}}}},
		fileStats: Stats{Tables: []StatsTable{{ID: "stats/table/public.orders",
			Table: "public.orders", Status: relStatusOK}},
			Columns: []ColumnStats{{ID: "stats/column/public.orders.token",
				Table: "public.orders", Column: "token", NullFrac: 0.25, NDistinct: -1,
				AvgWidth: 24, MostCommonVals: strPtr("{mcv-marker-7319,other-common-value}"),
				HistogramBounds: strPtr("{aaaa-bound,zzzz-bound}")}},
			ValueSamples: true, ColumnsListed: 1},
		fileContext: Context{Brief: ContextFact{ID: "context/brief", Type: "owner_brief",
			SubjectKind: "database", Subject: "*", Value: map[string]string{
				"growth": "steady, see 'quarter-secret-note'", "owner": "dba team"},
			ConfirmedBy: "owner@example.com", ConfirmedAt: "2026-10-01T00:00:00Z"},
			Facts: []ContextFact{}},
		fileSettings: Settings{Status: relStatusOK, Settings: []SettingFact{
			{ID: "setting/primary_conninfo", Name: "primary_conninfo",
				Value: "host=replica user=rep password=hunter2-conninfo",
				Source: "configuration file", Changes: []SettingChange{{Day: "2026-10-02",
					From: "", To: "host=replica user=rep password=hunter2-conninfo"}}},
			{ID: "setting/work_mem", Name: "work_mem", Value: "4096", Unit: "kB",
				Source: "default", Changes: []SettingChange{}}},
			Measures: measuresOf(Measures{}),
			Hardware: Hardware{VCPUs: valueUnknown, MemoryBytes: valueUnknown}},
	}
}

// modelFixture is the seeded local tier.
func modelFixture(t *testing.T) *Pack {
	t.Helper()
	w := fourWeeks()
	h := synthHistory(w)
	h.texts[1] = "SELECT * FROM orders WHERE token = 'sk-test-4f9a8b7c6d5e3f21' " +
		"AND card = 4111111111111111"
	return withSections(t, mustAssemble(t, synthRequest(w), h), fixtureCatalog())
}

// tierBytes is every file of p, names and contents, in one buffer.
func tierBytes(p *Pack) []byte {
	var b bytes.Buffer
	for _, f := range p.Files {
		b.WriteString(f.Name)
		b.WriteByte('\n')
		b.Write(f.Data)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

func mustForModel(t *testing.T, p *Pack, opts ModelOptions) *Pack {
	t.Helper()
	m, err := ForModel(p, opts)
	if err != nil {
		t.Fatalf("ForModel: %v", err)
	}
	return m
}

func testPseudonymKey() []byte { return bytes.Repeat([]byte{7}, 32) }
