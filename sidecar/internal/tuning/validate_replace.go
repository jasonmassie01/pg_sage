package tuning

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Roadmap 2.3: a candidate that makes exactly one existing index redundant
// is proposed as a replacement, one approved action that builds it and
// drops the old index (the executor's replace_index). The finding carries
// the statement pair, its undo, and the old index's identity (OID) and
// definition, which the executor checks before it drops anything.

// replacement is the snapshot's record of the index a candidate would
// replace, or why it cannot be replaced.
func (v *validator) replacement(p Proposal, spec optimizer.IndexSpec,
	old optimizer.IndexInfo) (collector.IndexStats, *Judged) {
	ix, found := v.findIndex(spec.TableSchema, old.Name)
	refuse := func(format string, args ...any) (collector.IndexStats, *Judged) {
		j := reject(p, ReasonSubsumes, "it would make %s redundant, but %s", old.Name,
			fmt.Sprintf(format, args...))
		return collector.IndexStats{}, &j
	}
	switch {
	case !found || ix.IndexRelID == 0:
		return refuse("its identity (OID) is not in the snapshot, so a replacement " +
			"cannot bind to it")
	case ix.IsUnique || ix.IsPrimary:
		return refuse("it enforces uniqueness and is never replaced")
	case !ix.IsValid:
		return refuse("it is invalid; the invalid-index rule handles it")
	case v.dropping[ix.IndexRelName]:
		return refuse("it is proposed for a drop this cycle")
	}
	return ix, nil
}

// admitReplacement turns an admitted create into the replacement of old.
func (v *validator) admitReplacement(p Proposal, f analyzer.Finding, table, create string,
	old collector.IndexStats, pred verify.Prediction) Judged {
	spec, err := optimizer.ParseIndexDDL(create)
	if err != nil {
		return reject(p, ReasonInvalid, "the admitted index is not a CREATE INDEX: %v", err)
	}
	oldIndex := qualified(old.SchemaName, old.IndexRelName)
	newIndex := qualified(spec.TableSchema, spec.Name)
	v.dropping[old.IndexRelName] = true
	d := make(map[string]any, len(f.Detail)+2)
	for k, val := range f.Detail {
		d[k] = val
	}
	delete(d, "drop_ddl") // the undo is the pair's rollback
	d["table"] = table
	d["index_replace"] = map[string]any{"old_index": oldIndex,
		"old_index_oid": int64(old.IndexRelID), "old_definition": old.IndexDef,
		"new_index": newIndex}
	f.Category, f.ObjectType, f.ObjectIdentifier = CategoryIndexReplace, "index", table
	f.Title = fmt.Sprintf("Replace index %s with %s on %s", oldIndex, newIndex, table)
	f.Detail = d
	f.Recommendation = strings.TrimSpace(p.Rationale + " (replaces " + oldIndex +
		", which the new index subsumes)")
	f.RecommendedSQL = optimizer.IndexReplaceSQL(create, oldIndex)
	f.RollbackSQL = optimizer.IndexReplaceRollbackSQL(old.IndexDef, newIndex)
	pred.Class = verify.ClassIndexReplace
	return Judged{Proposal: p, Verdict: VerdictAdmitted, Finding: &f, Tables: []string{table},
		Class: verify.ClassIndexReplace, Prediction: pred}
}

// replacedIndex is the canonical index a replacement finding would drop.
func replacedIndex(f analyzer.Finding) string {
	if f.Category != CategoryIndexReplace {
		return ""
	}
	rep, _ := f.Detail["index_replace"].(map[string]any)
	ix, _ := rep["old_index"].(string)
	return canonicalRef(ix)
}

// replacedIndexGone is why an open replacement is stale: the index it
// would replace no longer exists.
func replacedIndexGone(f analyzer.Finding, st CatalogState) string {
	ix := replacedIndex(f)
	if exists, known := st.Indexes[ix]; ix != "" && known && !exists {
		return "index " + ix + " it would replace no longer exists"
	}
	return ""
}
