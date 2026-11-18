package drill

import (
	"fmt"
	"math"
	"strings"
)

// RowTolerancePct is how far, in percent of the source's value, a clone's
// row estimate or exact count may be from the source's.
const RowTolerancePct = 5

// WithinTolerance reports whether clone is within RowTolerancePct of
// source: 5.0% passes, 5.1% fails. An empty source only matches an empty
// clone.
func WithinTolerance(source, clone float64) bool {
	return math.Abs(clone-source)*100 <= source*RowTolerancePct
}

// evaluate turns the facts into the invariants, in a fixed order: catalog,
// readable, row_estimate, row_count, sample_checksum, marker.
func evaluate(src sourceFacts, cl cloneFacts, exact exactFacts) []Invariant {
	var out []Invariant
	if len(src.tables) == 0 {
		out = append(out, Invariant{Name: "catalog", Status: StatusPass, Evidence: "no tables"})
	}
	for _, t := range src.tables {
		out = append(out, catalogInvariant(t, cl))
	}
	for _, t := range src.tables {
		if inv, ok := readableInvariant(t, cl); ok {
			out = append(out, inv)
		}
	}
	for _, t := range src.tables {
		if cl.sameShape(t) {
			out = append(out, estimateInvariant(t, cl.tables[t.key()]))
		}
	}
	for _, t := range src.tables {
		if n, ok := cl.counts[t.key()]; ok {
			out = append(out, countInvariant(t, exact.counts[t.key()], n))
		}
	}
	for _, t := range src.tables {
		if cl.sameShape(t) {
			out = append(out, sampleInvariant(t, cl, exact))
		}
	}
	return append(out, Invariant{Name: "marker", Status: cl.markerStatus,
		Evidence: cl.markerReason})
}

func catalogInvariant(t tableInfo, cl cloneFacts) Invariant {
	inv := Invariant{Name: "catalog", Table: t.key()}
	got, ok := cl.tables[t.key()]
	switch {
	case !ok:
		inv.Status, inv.Evidence = StatusFail, "missing on the clone"
	case !cl.sameShape(t):
		inv.Status = StatusFail
		inv.Evidence = fmt.Sprintf("columns differ: source (%s), clone (%s)",
			strings.Join(t.Columns, ", "), strings.Join(got.Columns, ", "))
	default:
		inv.Status = StatusPass
		inv.Evidence = fmt.Sprintf("%d columns match", len(t.Columns))
	}
	return inv
}

func readableInvariant(t tableInfo, cl cloneFacts) (Invariant, bool) {
	err, probed := cl.readable[t.key()]
	if !probed {
		return Invariant{}, false
	}
	if err != nil {
		return Invariant{Name: "readable", Table: t.key(), Status: StatusFail,
			Evidence: err.Error()}, true
	}
	return Invariant{Name: "readable", Table: t.key(), Status: StatusPass,
		Evidence: "SELECT 1 succeeded"}, true
}

func estimateInvariant(src, cl tableInfo) Invariant {
	inv := Invariant{Name: "row_estimate", Table: src.key()}
	if src.Tuples < 0 || cl.Tuples < 0 {
		inv.Status = StatusUnknown
		inv.Evidence = "not analyzed (reltuples -1) on the source or the clone"
		return inv
	}
	inv.Evidence = fmt.Sprintf("source %.0f, clone %.0f (within %d%%)", src.Tuples, cl.Tuples,
		RowTolerancePct)
	inv.Status = StatusPass
	if !WithinTolerance(src.Tuples, cl.Tuples) {
		inv.Status = StatusFail
		inv.Evidence = fmt.Sprintf("source %.0f, clone %.0f: more than %d%% apart",
			src.Tuples, cl.Tuples, RowTolerancePct)
	}
	return inv
}

func countInvariant(t tableInfo, source, cloneRows int64) Invariant {
	inv := Invariant{Name: "row_count", Table: t.key(), Status: StatusPass,
		Evidence: fmt.Sprintf("source %d rows, clone %d", source, cloneRows)}
	if !WithinTolerance(float64(source), float64(cloneRows)) {
		inv.Status = StatusFail
		inv.Evidence = fmt.Sprintf("source %d rows, clone %d: more than %d%% apart", source,
			cloneRows, RowTolerancePct)
	}
	return inv
}

func sampleInvariant(t tableInfo, cl cloneFacts, exact exactFacts) Invariant {
	inv := Invariant{Name: "sample_checksum", Table: t.key(), Status: StatusUnknown}
	if !sampled(t) {
		inv.Evidence = "no primary key and created_at-like column to sample by"
		return inv
	}
	got, onClone := cl.sums[t.key()]
	want, onSource := exact.sums[t.key()]
	switch {
	case !onClone || !onSource:
		inv.Evidence = "the sample could not be read"
	case want.Rows == 0:
		inv.Evidence = "no source rows created before the recovery point"
	case got == want:
		inv.Status = StatusPass
		inv.Evidence = fmt.Sprintf("%d rows, md5 %s", got.Rows, got.MD5)
	default:
		inv.Status = StatusFail
		inv.Evidence = fmt.Sprintf("source %d rows md5 %s, clone %d rows md5 %s", want.Rows,
			want.MD5, got.Rows, got.MD5)
	}
	return inv
}
