package partition

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

func ident(name string) string { return pgx.Identifier{name}.Sanitize() }

// convertSteps are the statements that turn the locked plain table into
// the partitioned layout, in order: the catalog changes before the ATTACH,
// then the ATTACH and what follows it. None reads the old heap: the bound
// is proven by the validated cutover CHECK, the key is pre-built.
func convertSteps(t Table, p convertPlan) (before, attach []string) {
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
		PARTITION BY RANGE (%s)`, t.ident(), hist, ident(t.Column)),
		// LIKE copied the cutover CHECK; the parent must not refuse later rows.
		fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s", t.ident(), ident(t.checkName())))
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
	return steps, cutoverSteps(t, p, hist)
}

// keySteps drop the plain table's primary key (a partitioned table's keys
// must include the day column), keep its columns NOT NULL (proven by the
// cutover CHECK: no scan) and make the pre-built key the history
// partition's primary key, which the ATTACH adopts instead of building one.
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
	if len(t.Key) > 0 {
		steps = append(steps, fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s PRIMARY KEY USING INDEX %s",
			hist, ident(t.HistoryName()+"_pkey"), ident(t.keyName())))
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

// cutoverSteps attach the old table as the history partition (its bound is
// implied by the validated cutover CHECK, so nothing is scanned; no row is
// dated at or past the cut, so none moves), drop the CHECK and add the
// empty default partition.
func cutoverSteps(t Table, p convertPlan, hist string) []string {
	return []string{
		fmt.Sprintf("ALTER TABLE %s ATTACH PARTITION %s FOR VALUES FROM (MINVALUE) TO (%s)",
			t.ident(), hist, literal(p.cutover)),
		fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s", hist, ident(t.checkName())),
		fmt.Sprintf("CREATE TABLE %s PARTITION OF %s DEFAULT", child(t.DefaultName()),
			t.ident()),
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
