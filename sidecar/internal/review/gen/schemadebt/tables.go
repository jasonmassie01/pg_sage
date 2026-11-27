package schemadebt

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// Queue and soft-delete churn: updated plus deleted rows over the window
// of at least churnMinRows, and at least churnMinShare of all writes.
const (
	churnMinRows  = 100000
	churnMinShare = 0.5
)

// Column names that mark a queue table and a soft-delete table.
var (
	queueColumns      = []string{"status", "state"}
	softDeleteColumns = []string{"deleted_at", "is_deleted", "deleted"}
)

// timestamp is a timestamp without time zone column's item: the online
// swap to timestamptz as advice carrying the planner's script when it
// drafts one, advice saying why not otherwise.
func (b *builder) timestamp(t tsDebt) error {
	col, missing := b.column(t.ref)
	script, refusal, err := b.swap(t.ref, col, "timestamp with time zone")
	if err != nil {
		return err
	}
	ev := append([]string{t.table.ID}, t.families...)
	reason := fmt.Sprintf("%s is timestamp without time zone and the window's top query "+
		"families read %s (%s): its values carry no zone, so a session TimeZone change "+
		"or daylight saving shifts what they mean. The swap casts each value in the "+
		"session's TimeZone: set TimeZone to the zone the application wrote them in "+
		"before the backfill.", t.ref, t.table.Table, strings.Join(t.families, ", "))
	c := candidate.Candidate{Kind: KindTimestampNoTZ, Target: t.ref.String(),
		ActionType: candidate.ActionAdvice, EvidenceIDs: ev,
		Prediction: candidate.Prediction{Metric: "column_type", Direction: "widen",
			Source: "schema section and workload families"}}
	if script != "" {
		c.ForwardSQL = script
		reason += " The swap is an online migration plan (expand, backfill, contract) " +
			"for a person to run; pg_sage never runs it."
	}
	c.Reason = withNotes(reason, missing, refusal)
	b.emit(c)
	return nil
}

// tables adds the giant table, queue and soft-delete items.
func (b *builder) tables(sch workpack.Schema, idx []workpack.IndexFact) {
	writes := tableWrites(idx)
	for _, t := range sch.Tables {
		b.largeTable(t)
		if w, ok := writes[t.Table]; ok {
			b.churnTable(t, w)
		}
	}
}

// largeTable advises range partitioning an unpartitioned table of at
// least LargeTableBytes that a date or timestamp key orders in time.
func (b *builder) largeTable(t workpack.TableFact) {
	key := timeOrderedKey(t)
	if t.Kind != "table" || t.SizeBytes < b.opts.LargeTableBytes || key == "" {
		return
	}
	b.emit(candidate.Candidate{Kind: KindLargeTable, Target: t.Table,
		ActionType: candidate.ActionAdvice, EvidenceIDs: []string{t.ID},
		Reason: fmt.Sprintf("%s is %d bytes (review.large_table_bytes is %d) and not "+
			"partitioned, and %s orders it in time: partition it by range on %s, so "+
			"old rows leave by dropping a partition instead of a bulk DELETE and vacuum. "+
			"The conversion rewrites the table (a new partitioned table, a copy, a swap); "+
			"a person plans and runs it, never pg_sage.", t.Table, t.SizeBytes,
			b.opts.LargeTableBytes, key, key),
		Prediction: candidate.Prediction{Metric: "table_bytes", Direction: "decrease",
			Source: "schema section", Quantities: map[string]float64{
				"table_bytes": float64(t.SizeBytes)}}})
}

// churn is a table's window writes and the index fact that states them.
type churn struct {
	ins, upd, del float64
	factID        string
}

// tableWrites are the window's writes by schema.table, from the first
// index fact of each table whose writes are known.
func tableWrites(idx []workpack.IndexFact) map[string]churn {
	out := map[string]churn{}
	for _, f := range idx {
		name := f.Schema + "." + f.Table
		m, ok := f.TableWrites.(map[string]any)
		if _, seen := out[name]; seen || !ok {
			continue
		}
		ins, okI := m["inserts"].(float64)
		upd, okU := m["updates"].(float64)
		del, okD := m["deletes"].(float64)
		if okI && okU && okD {
			out[name] = churn{ins: ins, upd: upd, del: del, factID: f.ID}
		}
	}
	return out
}

// churnTable advises on a queue or soft-delete table: one whose updated
// and deleted rows are most of its writes, with a status or deleted
// marker column.
func (b *builder) churnTable(t workpack.TableFact, w churn) {
	moved := w.upd + w.del
	if moved < churnMinRows || moved < churnMinShare*(w.ins+moved) {
		return
	}
	kind, advice := KindQueueTable, "a queue: keep the live rows small with a partial "+
		"index on the pending status, claim rows with FOR UPDATE SKIP LOCKED, and delete "+
		"or partition away finished rows instead of updating them in place"
	col, ok := hasColumn(t, queueColumns...)
	if !ok {
		if col, ok = hasColumn(t, softDeleteColumns...); !ok {
			return
		}
		kind, advice = KindSoftDelete, "soft-deleted rows: index the live rows with a "+
			"partial index on the marker, and archive or partition away deleted rows so "+
			"scans and vacuum stop paying for them"
	}
	b.emit(candidate.Candidate{Kind: kind, Target: t.Table,
		ActionType: candidate.ActionAdvice, EvidenceIDs: []string{t.ID, w.factID},
		Reason: fmt.Sprintf("%s updated or deleted %s rows over the window against %s "+
			"inserted, and its %s column marks %s.", t.Table, num(moved), num(w.ins), col,
			advice),
		Prediction: candidate.Prediction{Metric: "dead_tuples", Direction: "decrease",
			Source: "index section table writes", Quantities: map[string]float64{
				"updated_or_deleted_rows": moved}}})
}
