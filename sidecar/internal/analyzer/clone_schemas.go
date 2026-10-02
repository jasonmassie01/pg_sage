package analyzer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/collector"
)

// CategoryCloneSchemas is one family of schemas that are copies of one
// shape (dogfood lifeos-1: 477 of 478 open duplicate_index findings were
// in leaked test schemas test_memory_<hash>, each a copy of the same
// tables). A family's findings are reported once, here.
const CategoryCloneSchemas = "clone_schemas"

// cloneFamilyMin is the fewest copies that make a family.
const cloneFamilyMin = 5

// minCloneSuffix is the shortest generated suffix (hash, number, date).
const minCloneSuffix = 6

// cloneStem returns a schema name's stem when the name ends in a
// generated suffix (hex digits, digits and separators, at least
// minCloneSuffix long, with a digit), else "".
func cloneStem(name string) string {
	i := len(name)
	for i > 0 && isCloneSuffixByte(name[i-1]) {
		i--
	}
	for i < len(name) && (name[i] == '_' || name[i] == '-') {
		i++
	}
	suffix := name[i:]
	if i == 0 || len(suffix) < minCloneSuffix || !strings.ContainsAny(suffix, "0123456789") {
		return ""
	}
	return name[:i]
}

func isCloneSuffixByte(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F' ||
		b == '_' || b == '-'
}

// cloneFamilies maps each schema in a family to its family key (stem and
// a short hash of its table names). A schema's shape is its sorted table
// names; only schemas with a generated suffix can be clones.
func cloneFamilies(snap *collector.Snapshot) map[string]string {
	if snap == nil {
		return nil
	}
	tables := map[string][]string{}
	for _, t := range snap.Tables {
		tables[t.SchemaName] = append(tables[t.SchemaName], t.RelName)
	}
	members := map[string][]string{}
	for schema, names := range tables {
		stem := cloneStem(schema)
		if stem == "" {
			continue
		}
		sort.Strings(names)
		sum := sha256.Sum256([]byte(strings.Join(names, "\x1f")))
		key := stem + "*:" + hex.EncodeToString(sum[:4])
		members[key] = append(members[key], schema)
	}
	out := map[string]string{}
	for key, schemas := range members {
		if len(schemas) < cloneFamilyMin {
			continue
		}
		for _, s := range schemas {
			out[s] = key
		}
	}
	return out
}

// collapseCloneSchemas replaces the findings on objects in clone-family
// schemas with one clone_schemas finding per family.
func collapseCloneSchemas(snap *collector.Snapshot, findings []Finding) []Finding {
	families := cloneFamilies(snap)
	if len(families) == 0 {
		return findings
	}
	out := make([]Finding, 0, len(findings))
	collapsed := map[string]map[string]int{}
	schemas := map[string]map[string]bool{}
	for _, f := range findings {
		schema, _, ok := strings.Cut(f.ObjectIdentifier, ".")
		key, member := families[schema]
		if !ok || !member {
			out = append(out, f)
			continue
		}
		if collapsed[key] == nil {
			collapsed[key], schemas[key] = map[string]int{}, map[string]bool{}
		}
		collapsed[key][f.Category]++
		schemas[key][schema] = true
	}
	keys := make([]string, 0, len(collapsed))
	for k := range collapsed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, cloneFamilyFinding(k, familySize(families, k), collapsed[k],
			schemas[k]))
	}
	return out
}

func familySize(families map[string]string, key string) int {
	n := 0
	for _, k := range families {
		if k == key {
			n++
		}
	}
	return n
}

func cloneFamilyFinding(key string, size int, byCategory map[string]int,
	withFindings map[string]bool) Finding {
	total := 0
	for _, n := range byCategory {
		total += n
	}
	examples := make([]string, 0, len(withFindings))
	for s := range withFindings {
		examples = append(examples, s)
	}
	sort.Strings(examples)
	if len(examples) > 3 {
		examples = examples[:3]
	}
	stem, _, _ := strings.Cut(key, "*")
	return Finding{
		Category: CategoryCloneSchemas, Severity: "info", ObjectType: "schema_family",
		ObjectIdentifier: key,
		Title: fmt.Sprintf("%d schemas share one shape (%s…): %d findings reported once "+
			"here", size, stem, total),
		Detail: map[string]any{"schemas": size, "collapsed_findings": total,
			"by_category": byCategory, "examples": examples},
		Recommendation: "These schemas have the same tables and generated names, so they " +
			"look like copies (leftover test or clone schemas). If they are not in use, " +
			"drop them; pg_sage reports their findings once here instead of once per " +
			"schema.",
		ActionRisk: "safe",
	}
}
