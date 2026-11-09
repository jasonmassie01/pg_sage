package firstlook

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pg-sage/sidecar/internal/sqlident"
)

// leftoverSuffix matches the copies a failed or cancelled REINDEX
// CONCURRENTLY leaves behind: name_ccnew[N], the new copy it was building,
// and name_ccold[N], the old copy it swapped out but did not drop.
var leftoverSuffix = regexp.MustCompile(`_cc(new|old)[0-9]*$`)

// maxDuplicateKeys bounds the suggested duplicate-key query.
const maxDuplicateKeys = 20

// IndexBuild is one row of pg_stat_progress_create_index in this database.
type IndexBuild struct {
	// TableOID is relid. IndexOID is index_relid: 0 while a CREATE INDEX
	// that is not concurrent builds. REINDEX CONCURRENTLY names the original
	// while it waits for writers and the invalid _ccnew copy later, so a
	// match on its table counts as live.
	TableOID, IndexOID uint32
	Command, Phase     string
}

// IndexBuilds are the index builds in progress the first look could see.
type IndexBuilds struct {
	Builds []IndexBuild
	// Unseen says why some builds may be missing: the read failed, or
	// other roles' builds are hidden. "" when every build was seen.
	Unseen string
}

// building returns the build that explains x being invalid, if any.
func (b IndexBuilds) building(x Index) (IndexBuild, bool) {
	for _, bd := range b.Builds {
		if bd.IndexOID == x.OID {
			return bd, true
		}
		if bd.TableOID == x.TableOID &&
			(bd.IndexOID == 0 || bd.Command == "REINDEX CONCURRENTLY") {
			return bd, true
		}
	}
	return IndexBuild{}, false
}

// InvalidIndexes reports indexes with pg_index.indisvalid = false. A build
// in progress is info with no SQL. A partitioned index gets indexes to
// attach to its partitions, a leftover of a failed REINDEX CONCURRENTLY is
// dropped, an invalid unique index looks for its duplicate keys first, and
// any other failed build is rebuilt.
func InvalidIndexes(idx []Index, b IndexBuilds) []Item {
	var out []Item
	for _, x := range idx {
		if x.Valid {
			continue
		}
		if bd, ok := b.building(x); ok {
			out = append(out, buildingItem(x, bd))
			continue
		}
		it := failedItem(x, idx)
		if b.Unseen != "" {
			it.Caveat = "pg_sage could not see every index build in progress (" + b.Unseen +
				"). If one of them is building this index, it is not a failure: check " +
				"pg_stat_progress_create_index before acting."
		}
		out = append(out, it)
	}
	return out
}

func invalidEvidence(x Index) Evidence {
	return Evidence{Source: "pg_index",
		Ref:    "pg_index.indisvalid = false (indexrelid " + x.object() + ")",
		Detail: fmt.Sprintf("indisready=%t", x.Ready)}
}

func buildingItem(x Index, bd IndexBuild) Item {
	command := bd.Command
	if command == "" {
		command = "an index build"
	}
	return Item{Rule: RuleInvalidIndex, Severity: SeverityInfo, Object: x.object(),
		Title: "Index build in progress on " + x.object(),
		Detail: fmt.Sprintf("Index %s on %s (%s) is invalid because %s is building it "+
			"right now (phase: %s). It becomes valid when the build finishes.", x.object(),
			qualified(x.Schema, x.Table), sizeText(x.SizeBytes), command, bd.Phase),
		Recommendation: "Nothing to do while it builds. If the build fails or is " +
			"cancelled, pg_sage's invalid-index rule reports what it left behind.",
		Evidence: []Evidence{invalidEvidence(x), {Source: "pg_stat_progress_create_index",
			Ref: fmt.Sprintf("pg_stat_progress_create_index (relid %d, index_relid %d)",
				bd.TableOID, bd.IndexOID),
			Detail: fmt.Sprintf("command=%s phase=%s", bd.Command, bd.Phase)}}}
}

func reindexSQL(name string) string { return "REINDEX INDEX CONCURRENTLY " + name + ";" }

func failedItem(x Index, idx []Index) Item {
	name := sqlident.Qualified(x.Schema, x.Name)
	it := Item{Rule: RuleInvalidIndex, Severity: SeverityWarning, Object: x.object(),
		Title: "Invalid index " + x.object(), Evidence: []Evidence{invalidEvidence(x)}}
	// on reads "<index> on <table> is invalid (<size>)", today's wording.
	on := fmt.Sprintf("%s on %s is invalid (%s)", x.object(), qualified(x.Schema, x.Table),
		sizeText(x.SizeBytes))
	switch {
	case x.Partitioned:
		partitionedInvalidText(&it, x, on)
	case leftoverSuffix.MatchString(x.Name):
		leftoverText(&it, x, idx, on, name)
	case x.Unique:
		it.Detail = "Unique index " + on + ": its build failed, most often " +
			"because the table holds duplicate keys, and a rebuild fails again until they " +
			"are gone. It is maintained on every write (while ready it still rejects new " +
			"duplicates) but never used for reads."
		it.SuggestedSQL = duplicateKeysSQL(x)
		find := "Find and remove the duplicate keys first"
		if it.SuggestedSQL != "" {
			find += fmt.Sprintf(" (the suggested query lists up to %d)", maxDuplicateKeys)
		}
		it.Recommendation = find + ", or drop the index if it is not needed. Only then " +
			"rebuild it: " + reindexSQL(name)
	default:
		it.Detail = "Index " + on + ": a failed CREATE INDEX CONCURRENTLY or " +
			"REINDEX left it behind. It is maintained on every write but never used for reads."
		it.Recommendation = "Rebuild it if it is needed, otherwise drop it."
		it.SuggestedSQL = reindexSQL(name)
	}
	return it
}

// leftoverText fills a _ccnew or _ccold item: rebuilding the copy only
// makes another one, so the fix is to drop it.
func leftoverText(it *Item, x Index, idx []Index, on, name string) {
	m := leftoverSuffix.FindStringSubmatch(x.Name)
	what := "the new copy a REINDEX CONCURRENTLY was building when it failed or was cancelled"
	if m[1] == "old" {
		what = "the old copy a REINDEX CONCURRENTLY swapped out but did not drop"
	}
	it.Detail = "Index " + on + ": it is " + what + ". Rebuilding it only " +
		"makes another copy. It is maintained on every write but never used for reads."
	it.Recommendation = "Drop the leftover copy."
	base := strings.TrimSuffix(x.Name, m[0])
	for _, o := range idx {
		if m[1] == "new" && o.OID != x.OID && o.TableOID == x.TableOID &&
			o.Schema == x.Schema && o.Name == base {
			it.Recommendation += " To finish the rebuild, re-run " +
				reindexSQL(sqlident.Qualified(o.Schema, o.Name))
			break
		}
	}
	it.SuggestedSQL = "DROP INDEX CONCURRENTLY " + name + ";"
}

// duplicateKeysSQL is a read-only, bounded query for the keys that stop a
// unique index from building; "" unless every key column is a plain,
// named column. NULL keys are distinct unless NULLS NOT DISTINCT, and a
// partial index only constrains the rows of its predicate.
func duplicateKeysSQL(x Index) string {
	keys := x.keys()
	if x.Expressions != "" || len(x.KeyNames) == 0 || len(x.KeyNames) != len(keys) {
		return ""
	}
	cols := make([]string, len(keys))
	var where []string
	for i, n := range x.KeyNames {
		if n == "" || keys[i] == 0 {
			return ""
		}
		cols[i] = sqlident.Ident(n)
		if !x.NullsNotDistinct {
			where = append(where, cols[i]+" IS NOT NULL")
		}
	}
	if x.Predicate != "" {
		where = append(where, "("+x.Predicate+")")
	}
	list := strings.Join(cols, ", ")
	q := "SELECT " + list + ", count(*) AS copies FROM " +
		sqlident.Qualified(x.Schema, x.Table)
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	return q + fmt.Sprintf(" GROUP BY %s HAVING count(*) > 1 ORDER BY count(*) DESC "+
		"LIMIT %d;", list, maxDuplicateKeys)
}
