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
// tables). An idle family's findings are reported once, here; a live
// family (e.g. schema-per-tenant) has each issue fanned out once with
// the list of affected schemas (Phase 0 item 10).
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

// familyMembers inverts cloneFamilies: family key -> sorted schemas.
func familyMembers(families map[string]string) map[string][]string {
	out := map[string][]string{}
	for schema, key := range families {
		out[key] = append(out[key], schema)
	}
	for _, schemas := range out {
		sort.Strings(schemas)
	}
	return out
}

// collapseCloneSchemas reports the findings on objects in clone-family
// schemas per family. An idle family is a leftover: its findings are
// replaced by one clone_schemas finding. A live family keeps every
// distinct issue once, fanned out with its affected schemas, plus one
// informational schema-family finding.
func collapseCloneSchemas(snap *collector.Snapshot, findings []Finding,
	sig cloneSignals) []Finding {
	families := cloneFamilies(snap)
	if len(families) == 0 {
		return findings
	}
	out := make([]Finding, 0, len(findings))
	byFamily := map[string][]Finding{}
	for _, f := range findings {
		schema, _, ok := strings.Cut(f.ObjectIdentifier, ".")
		key, member := families[schema]
		if !ok || !member {
			out = append(out, f)
			continue
		}
		byFamily[key] = append(byFamily[key], f)
	}
	members := familyMembers(families)
	keys := make([]string, 0, len(byFamily))
	for k := range byFamily {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		idle, reason := familyIdle(snap, k, members[k], sig)
		if idle {
			out = append(out, leftoverFamilyFinding(k, len(members[k]), byFamily[k], reason))
			continue
		}
		issues := fanOutFamily(k, byFamily[k])
		out = append(out, issues...)
		out = append(out, liveFamilyFinding(k, len(members[k]), len(issues), reason))
	}
	return out
}

func leftoverFamilyFinding(key string, size int, findings []Finding,
	reason string) Finding {
	byCategory := map[string]int{}
	withFindings := map[string]bool{}
	for _, f := range findings {
		byCategory[f.Category]++
		schema, _, _ := strings.Cut(f.ObjectIdentifier, ".")
		withFindings[schema] = true
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
		Title: fmt.Sprintf("%d schemas share one shape (%s…): idle leftover copies, "+
			"%d findings reported once here", size, stem, len(findings)),
		Detail: map[string]any{"family_kind": "leftover", "schemas": size,
			"collapsed_findings": len(findings), "by_category": byCategory,
			"examples": examples, "activity": reason},
		Recommendation: "These schemas have the same tables and generated names and " +
			"are idle (" + reason + "), so they look like leftover test or clone " +
			"copies. If they are not needed, drop them; pg_sage never drops schemas " +
			"itself and reports their findings once here instead of once per schema.",
		ActionRisk: "safe",
	}
}

func liveFamilyFinding(key string, size, issues int, reason string) Finding {
	stem, _, _ := strings.Cut(key, "*")
	return Finding{
		Category: CategoryCloneSchemas, Severity: "info", ObjectType: "schema_family",
		ObjectIdentifier: key,
		Title: fmt.Sprintf("%d schemas share one shape (%s…): schema family in use, "+
			"%d issues reported once each", size, stem, issues),
		Detail: map[string]any{"family_kind": "schema_family", "schemas": size,
			"fanned_out_issues": issues, "activity": reason},
		Recommendation: "These schemas share one table layout and are in use (" +
			reason + "), for example one schema per tenant. Each issue is reported " +
			"once, on one schema, with the list of affected schemas: apply the fix to " +
			"every schema, or to the template the schemas are created from.",
		ActionRisk: "safe",
	}
}
