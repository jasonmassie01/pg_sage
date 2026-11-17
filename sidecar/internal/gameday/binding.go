package gameday

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/collector"
)

// benches-12: the bench's fixtures create their own bench_* tables, so a
// game day that ran only them said nothing about the customer's database.
// A customer program exercises one of the customer's own tables on the
// clone; its binding names that table, chosen from the monitored
// database's two newest collector snapshots.

// ProgramCustomerHotTable is the customer program that queues lock
// requests on the monitored database's hottest table (on the clone).
const ProgramCustomerHotTable = "lock_customer_hot_table"

// Reasons a customer program has no binding (it is then skipped).
const (
	SkipLocalProvider   = "the local provider's target holds no customer objects"
	SkipProgramsOff     = "sre.autonomy.game_days.customer_programs is off"
	SkipNoSnapshot      = "no collector snapshot of the monitored database yet"
	SkipOneSnapshot     = "one collector snapshot only: no activity delta to pick a table by"
	SkipNoTableStats    = "the collector could not read table statistics in the last two snapshots"
	SkipNoCustomerTable = "no permanent customer table in the collector snapshots"
	SkipNoActivity      = "no customer table had activity between the last two collector " +
		"snapshots"
)

// Binding is the customer table a customer program exercises on the
// clone, or why it has none (Skip). The name stays in the local game-day
// row; anything exported carries Hash instead.
type Binding struct {
	Program string `json:"program"`
	Schema  string `json:"schema,omitempty"`
	Table   string `json:"table,omitempty"`
	Skip    string `json:"skip,omitempty"`
}

// Bound reports whether the binding names a table.
func (b Binding) Bound() bool { return b.Skip == "" && b.Table != "" && b.Schema != "" }

// Ident is the table's quoted, schema-qualified identifier ("" unbound).
func (b Binding) Ident() string {
	if !b.Bound() {
		return ""
	}
	return pgx.Identifier{b.Schema, b.Table}.Sanitize()
}

// Hash identifies the table without naming it: sha256 of the schema and
// table (NUL-separated), first 16 hex digits ("" unbound).
func (b Binding) Hash() string {
	if !b.Bound() {
		return ""
	}
	sum := sha256.Sum256([]byte(b.Schema + "\x00" + b.Table))
	return "sha256:" + hex.EncodeToString(sum[:8])
}

// TableSnapshots is the monitored database's collector (its two newest
// snapshots).
type TableSnapshots interface {
	LatestSnapshot() *collector.Snapshot
	PreviousSnapshot() *collector.Snapshot
}

// HoldsCustomerObjects reports whether provider's clones are copies of the
// monitored database (a DLE clone, a Lakebase or Neon branch). The local
// provider, and any provider not known to copy it, holds none.
func HoldsCustomerObjects(provider string) bool {
	switch provider {
	case "dle", "lakebase_branch", "neon_branch":
		return true
	}
	return false
}

// HotTableBinding binds the customer hot-table program to the permanent
// leaf table with the largest delta of n_tup_ins + n_tup_upd + n_tup_del +
// idx_scan between previous and latest (lifetime counters say nothing about
// now). Ties go to the larger table, then the smaller qualified name. A
// table missing from previous, or whose counters went backwards (a stats
// reset), has no delta.
func HotTableBinding(latest, previous *collector.Snapshot) Binding {
	b := Binding{Program: ProgramCustomerHotTable}
	switch {
	case latest == nil && previous == nil:
		b.Skip = SkipNoSnapshot
		return b
	case latest == nil || previous == nil:
		b.Skip = SkipOneSnapshot
		return b
	case !latest.Available("tables") || !previous.Available("tables"):
		b.Skip = SkipNoTableStats
		return b
	}
	cands := hotCandidates(latest, previous)
	if len(cands) == 0 {
		b.Skip = SkipNoActivity
		if !anyCustomerTable(latest.Tables) {
			b.Skip = SkipNoCustomerTable
		}
		return b
	}
	best := cands[0]
	b.Schema, b.Table = best.t.SchemaName, best.t.RelName
	return b
}

type hotCandidate struct {
	t     collector.TableStats
	delta int64
}

// hotCandidates are the customer tables with a positive activity delta,
// hottest first.
func hotCandidates(latest, previous *collector.Snapshot) []hotCandidate {
	before := map[[2]string]collector.TableStats{}
	for _, t := range previous.Tables {
		before[[2]string{t.SchemaName, t.RelName}] = t
	}
	var out []hotCandidate
	for _, t := range latest.Tables {
		p, ok := before[[2]string{t.SchemaName, t.RelName}]
		if !ok || !customerTable(t) {
			continue
		}
		// Any counter that went backwards is a stats reset: no delta.
		monotonic := t.NTupIns >= p.NTupIns && t.NTupUpd >= p.NTupUpd &&
			t.NTupDel >= p.NTupDel && t.IdxScan >= p.IdxScan
		if d := activity(t) - activity(p); monotonic && d > 0 {
			out = append(out, hotCandidate{t: t, delta: d})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.delta != b.delta {
			return a.delta > b.delta
		}
		if a.t.TotalBytes != b.t.TotalBytes {
			return a.t.TotalBytes > b.t.TotalBytes
		}
		if a.t.SchemaName != b.t.SchemaName {
			return a.t.SchemaName < b.t.SchemaName
		}
		return a.t.RelName < b.t.RelName
	})
	return out
}

func activity(t collector.TableStats) int64 {
	return t.NTupIns + t.NTupUpd + t.NTupDel + t.IdxScan
}

// customerTable is a permanent ordinary table outside pg_sage's schema and
// the system schemas.
func customerTable(t collector.TableStats) bool {
	s := t.SchemaName
	if s == "" || t.RelName == "" || s == "sage" || s == "information_schema" ||
		strings.HasPrefix(s, "pg_") {
		return false
	}
	return t.Relpersistence == "p" && t.Relkind == "r"
}

func anyCustomerTable(ts []collector.TableStats) bool {
	for _, t := range ts {
		if customerTable(t) {
			return true
		}
	}
	return false
}
