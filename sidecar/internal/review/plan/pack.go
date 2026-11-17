package plan

import (
	"cmp"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/workload"
)

// maxTargetQueries bounds a step's measured statements (the hottest).
const maxTargetQueries = 20

// localPack is what the plan reads from the pack's local tier: tables
// (by "schema.table" as the pack writes them), indexes, settings and the
// window's statements with their texts.
type localPack struct {
	pack     *workpack.Pack
	tables   map[string]workpack.TableFact
	indexes  map[string]workpack.IndexFact
	settings map[string]workpack.SettingFact
	queries  []packQuery
	known    map[string][]string
}

// packQuery is one statement of the window.
type packQuery struct {
	id     int64
	text   string
	calls  int64
	execMs float64
}

func readPack(p *workpack.Pack) (*localPack, error) {
	lp := &localPack{pack: p, tables: map[string]workpack.TableFact{},
		indexes: map[string]workpack.IndexFact{}, settings: map[string]workpack.SettingFact{},
		known: map[string][]string{}}
	var sch workpack.Schema
	var idx workpack.Indexes
	var set workpack.Settings
	for name, into := range map[string]any{"schema.json": &sch, "indexes.json": &idx,
		"settings.json": &set} {
		if raw := p.File(name); raw != nil {
			if err := json.Unmarshal(raw, into); err != nil {
				return nil, fmt.Errorf("decode the pack's %s: %w", name, err)
			}
		}
	}
	for _, t := range sch.Tables {
		lp.tables[t.Table] = t
		if schema, name, ok := strings.Cut(t.Table, "."); ok {
			lp.addKnown(schema, name)
		}
	}
	for _, f := range idx.Indexes {
		lp.indexes[f.Schema+"."+f.Index] = f
		lp.addKnown(f.Schema, f.Table)
	}
	for _, s := range set.Settings {
		lp.settings[strings.ToLower(s.Name)] = s
	}
	q, err := readQueries(p)
	if err != nil {
		return nil, err
	}
	lp.queries = q
	return lp, nil
}

func (lp *localPack) addKnown(schema, table string) {
	if !slices.Contains(lp.known[table], schema) {
		lp.known[table] = append(lp.known[table], schema)
	}
}

// readQueries lists the window's statements that have a text, hottest
// first (exec time, then queryid).
func readQueries(p *workpack.Pack) ([]packQuery, error) {
	var wl workpack.Workload
	var texts workpack.Texts
	if raw := p.File("workload.json"); raw != nil {
		if err := json.Unmarshal(raw, &wl); err != nil {
			return nil, fmt.Errorf("decode the pack's workload.json: %w", err)
		}
	}
	if raw := p.File("texts.json"); raw != nil {
		if err := json.Unmarshal(raw, &texts); err != nil {
			return nil, fmt.Errorf("decode the pack's texts.json: %w", err)
		}
	}
	byID := map[string]string{}
	for _, t := range texts.Texts {
		byID[t.QueryID] = t.Text
	}
	var out []packQuery
	for _, fam := range wl.Families {
		for _, q := range fam.QueryIDs {
			text, ok := byID[q.QueryID]
			id, err := strconv.ParseInt(q.QueryID, 10, 64)
			if ok && err == nil {
				out = append(out, packQuery{id: id, text: text, calls: q.Calls,
					execMs: q.ExecMs})
			}
		}
	}
	slices.SortStableFunc(out, func(a, b packQuery) int {
		return cmp.Or(cmp.Compare(b.execMs, a.execMs), cmp.Compare(a.id, b.id))
	})
	return out, nil
}

// tableQueries are the statements that read or write schema.table.
func (lp *localPack) tableQueries(schema, table string) []packQuery {
	var out []packQuery
	for _, q := range lp.queries {
		for _, r := range workload.ReferencedTables(q.text, lp.known) {
			if r.Schema == schema && r.Name == table {
				out = append(out, q)
				break
			}
		}
	}
	return out
}

// mentioning keeps the statements whose text names one of the columns.
func mentioning(qs []packQuery, columns []string) []packQuery {
	var out []packQuery
	for _, q := range qs {
		text := workload.BlankLiteralsAndComments(q.text)
		for _, c := range columns {
			if columnPattern(c).MatchString(text) {
				out = append(out, q)
				break
			}
		}
	}
	return out
}

// columnPattern matches a column as a whole word: a lower-case name in
// any case unquoted or quoted, any other name only quoted.
func columnPattern(c string) *regexp.Regexp {
	q := regexp.QuoteMeta(c)
	if c == strings.ToLower(c) && identBare.MatchString(c) {
		return regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_$])"?` + q + `"?($|[^A-Za-z0-9_$])`)
	}
	return regexp.MustCompile(`"` + q + `"`)
}

// writers keeps the INSERT, UPDATE, DELETE and MERGE statements.
func writers(qs []packQuery) []packQuery {
	var out []packQuery
	for _, q := range qs {
		head := strings.ToUpper(strings.TrimSpace(workload.BlankLiteralsAndComments(q.text)))
		for _, verb := range []string{"INSERT", "UPDATE", "DELETE", "MERGE"} {
			if strings.HasPrefix(head, verb) {
				out = append(out, q)
				break
			}
		}
	}
	return out
}

// hottest bounds qs to maxTargetQueries.
func hottest(qs []packQuery) []packQuery { return qs[:min(len(qs), maxTargetQueries)] }

func ids(qs []packQuery) []int64 {
	out := make([]int64, 0, len(qs))
	for _, q := range qs {
		out = append(out, q.id)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func calls(qs []packQuery) int64 {
	var n int64
	for _, q := range qs {
		n += q.calls
	}
	return n
}

func queryInfos(qs []packQuery) []optimizer.QueryInfo {
	out := make([]optimizer.QueryInfo, 0, len(qs))
	for _, q := range qs {
		info := optimizer.QueryInfo{QueryID: q.id, Text: q.text, Calls: q.calls,
			TotalTimeMs: q.execMs}
		if q.calls > 0 {
			info.MeanTimeMs = q.execMs / float64(q.calls)
		}
		out = append(out, info)
	}
	return out
}

// resolves keeps the evidence ids that resolve in the pack (a candidate
// alias such as cand:<id> never does).
func (lp *localPack) resolves(idsIn []string) []string {
	var out []string
	for _, id := range idsIn {
		if _, err := workpack.Resolve(lp.pack, id); err == nil && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// subject is a table a candidate's evidence names, with the key columns
// of the index it names (none for a table fact).
type subject struct {
	schema, table string
	columns       []string
}

// subjectsOf reads the tables (and index keys) a candidate's evidence ids
// name: schema table facts and index facts.
func (lp *localPack) subjectsOf(evidence []string) []subject {
	var out []subject
	for _, id := range evidence {
		if t, ok := lp.tableByID(id); ok {
			schema, name, _ := strings.Cut(t.Table, ".")
			out = append(out, subject{schema: schema, table: name})
		}
		for _, f := range lp.indexes {
			if f.ID == id {
				out = append(out, subject{schema: f.Schema, table: f.Table,
					columns: indexColumns(f.Definition)})
			}
		}
	}
	return out
}

func (lp *localPack) tableByID(id string) (workpack.TableFact, bool) {
	for _, t := range lp.tables {
		if t.ID == id {
			return t, true
		}
	}
	return workpack.TableFact{}, false
}

var reIndexKeys = regexp.MustCompile(`(?i)\busing\s+\w+\s*\(([^()]*)\)`)

// indexColumns are the plain key columns of an index definition.
func indexColumns(def string) []string {
	m := reIndexKeys.FindStringSubmatch(def)
	if m == nil {
		return nil
	}
	var out []string
	for _, part := range strings.Split(m[1], ",") {
		name := strings.Fields(strings.TrimSpace(part))
		if len(name) > 0 {
			out = append(out, strings.Trim(name[0], `"`))
		}
	}
	return out
}
