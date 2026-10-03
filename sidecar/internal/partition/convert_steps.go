package partition

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

func ident(name string) string { return pgx.Identifier{name}.Sanitize() }

// convertSteps are the statements that turn the locked plain table into
// the partitioned layout, in order.
func convertSteps(t Table, p convertPlan) []string {
	hist := child(t.HistoryName())
	steps := []string{fmt.Sprintf("ALTER TABLE %s RENAME TO %s", t.ident(),
		ident(t.HistoryName()))}
	steps = append(steps, keySteps(t, p, hist)...)
	steps = append(steps, indexRenames(t, p)...)
	for _, col := range sortedKeys(p.sequences) {
		steps = append(steps, "ALTER SEQUENCE "+p.sequences[col]+" OWNED BY NONE")
	}
	steps = append(steps, fmt.Sprintf(`CREATE TABLE %s (LIKE %s INCLUDING DEFAULTS
		INCLUDING CONSTRAINTS INCLUDING STORAGE INCLUDING COMMENTS)
		PARTITION BY RANGE (%s)`, t.ident(), hist, ident(t.Column)))
	if len(t.Key) > 0 {
		cols := make([]string, 0, len(t.Key)+1)
		for _, k := range append(append([]string{}, t.Key...), t.Column) {
			cols = append(cols, ident(k))
		}
		steps = append(steps, fmt.Sprintf("ALTER TABLE %s ADD PRIMARY KEY (%s)", t.ident(),
			strings.Join(cols, ", ")))
	}
	for _, ix := range p.indexes {
		if ix.constraint == "" {
			steps = append(steps, ix.def) // names sage.<table>: now the partitioned one
		}
	}
	for _, col := range sortedKeys(p.sequences) {
		steps = append(steps, fmt.Sprintf("ALTER SEQUENCE %s OWNED BY %s.%s", p.sequences[col],
			t.ident(), ident(col)))
	}
	steps = append(steps, grantSteps(t, p.grants)...)
	return append(steps, cutoverSteps(t, p, hist)...)
}

// keySteps drop the plain table's primary key (a partitioned table's keys
// must include the day column) and keep its columns NOT NULL.
func keySteps(t Table, p convertPlan, hist string) []string {
	var steps []string
	for _, ix := range p.indexes {
		if ix.constraint != "" {
			steps = append(steps, fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s", hist,
				ident(ix.constraint)))
		}
	}
	for _, k := range t.Key {
		steps = append(steps, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL", hist,
			ident(k)))
	}
	return steps
}

// indexRenames free the secondary index names for the partitioned table.
func indexRenames(t Table, p convertPlan) []string {
	var steps []string
	n := 0
	for _, ix := range p.indexes {
		if ix.constraint != "" {
			continue
		}
		n++
		steps = append(steps, fmt.Sprintf("ALTER INDEX %s RENAME TO %s", child(ix.name),
			ident(fmt.Sprintf("%s_i%d", t.HistoryName(), n))))
	}
	return steps
}

func grantSteps(t Table, grants []grant) []string {
	steps := make([]string, 0, len(grants))
	for _, g := range grants {
		who := "PUBLIC"
		if g.grantee != "PUBLIC" {
			who = ident(g.grantee)
		}
		opt := ""
		if g.grantable {
			opt = " WITH GRANT OPTION"
		}
		steps = append(steps, fmt.Sprintf("GRANT %s ON %s TO %s%s", g.privilege, t.ident(),
			who, opt))
	}
	return steps
}

// cutoverSteps create the default partition, move rows dated on or after
// the cutover into it, and attach the old table as the history partition.
// Attaching validates the bound with one read of the old table's heap.
func cutoverSteps(t Table, p convertPlan, hist string) []string {
	def, col, cut := child(t.DefaultName()), ident(t.Column), literal(p.cutover)
	return []string{
		fmt.Sprintf("CREATE TABLE %s PARTITION OF %s DEFAULT", def, t.ident()),
		fmt.Sprintf("INSERT INTO %s SELECT * FROM %s WHERE %s >= %s", def, hist, col, cut),
		fmt.Sprintf("DELETE FROM %s WHERE %s >= %s", hist, col, cut),
		fmt.Sprintf("ALTER TABLE %s ATTACH PARTITION %s FOR VALUES FROM (MINVALUE) TO (%s)",
			t.ident(), hist, cut),
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
