// Package snapfixture generates deterministic collector snapshot histories
// for tests of the snapshot store. The same history is written once in the
// legacy format (one full jsonb row per category and cycle) and once
// through the delta writer, and every consumer must read identical results
// from both (the golden comparison).
package snapfixture

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/collector"
)

// Doc is one category document of a cycle, marshaled exactly as the
// collector marshals it.
type Doc struct {
	Category string
	Data     []byte
}

// Cycle is one collection cycle: the instant and its category documents,
// sorted by category. A category that was unavailable has no document.
type Cycle struct {
	At   time.Time
	Docs []Doc
}

// Events schedules catalog changes by cycle number (1-based; 0 = never).
type Events struct {
	// CounterReset zeroes every index and table counter (pg_stat_reset).
	CounterReset int
	// DropIndex drops the first secondary index; it comes back two cycles
	// later with a different definition (drop and recreate).
	DropIndex int
	// Redefine gives the second secondary index a new definition and
	// access method under the same name (DDL change mid-window).
	Redefine int
	// Rename renames the third secondary index in place (same position).
	Rename int
	// EmptyFrom..EmptyTo (exclusive) is a span with no indexes at all:
	// the collector writes null for an empty catalog.
	EmptyFrom, EmptyTo int
	// Unavailable is a cycle whose index read failed: no indexes row.
	Unavailable int
	// QueryChurn evicts one query and admits a new one every N cycles.
	QueryChurn int
}

// Scenario describes a deterministic history.
type Scenario struct {
	Start   time.Time
	Step    time.Duration
	Cycles  int
	Tables  int
	Indexes int // secondary indexes, spread over the tables
	Seed    int64
	Events  Events
}

// Execer runs one statement (pgxpool.Pool, pgx.Conn and pgx.Tx satisfy it).
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// InsertLegacy writes a cycle the way the collector did before the delta
// format: one full row per category.
func InsertLegacy(ctx context.Context, db Execer, c Cycle) error {
	for _, d := range c.Docs {
		if _, err := db.Exec(ctx, `INSERT INTO sage.snapshots
			(collected_at, category, data) VALUES ($1, $2, $3)`,
			c.At, d.Category, d.Data); err != nil {
			return fmt.Errorf("insert legacy %s snapshot: %w", d.Category, err)
		}
	}
	return nil
}

// Generate builds the scenario's cycles.
func (s Scenario) Generate() ([]Cycle, error) {
	if s.Cycles <= 0 || s.Step <= 0 || s.Tables <= 0 {
		return nil, fmt.Errorf("scenario needs cycles, a step and tables: %+v", s)
	}
	st := newState(s)
	out := make([]Cycle, 0, s.Cycles)
	for i := 1; i <= s.Cycles; i++ {
		st.advance(i)
		c, err := st.cycle(s.Start.Add(time.Duration(i-1)*s.Step), i)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// state is the evolving catalog.
type state struct {
	sc      Scenario
	rng     *rand.Rand
	tables  []collector.TableStats
	indexes []collector.IndexStats
	dropped *collector.IndexStats
	seqs    []collector.SequenceStats
	queries []collector.QueryStats
	nextQID int64
	fks     []collector.ForeignKey
	parts   []collector.PartitionInfo
	system  collector.SystemStats
	config  collector.ConfigSnapshot
	wal     int64
}

func newState(s Scenario) *state {
	st := &state{sc: s, rng: rand.New(rand.NewSource(s.Seed)), nextQID: 1 << 40}
	for t := 0; t < s.Tables; t++ {
		st.tables = append(st.tables, newTable(t))
		st.indexes = append(st.indexes, primaryKey(t))
	}
	for i := 0; i < s.Indexes; i++ {
		st.indexes = append(st.indexes, secondaryIndex(i%s.Tables, i))
	}
	for i := 0; i < 40; i++ {
		st.seqs = append(st.seqs, collector.SequenceStats{SchemaName: "app",
			SequenceName: fmt.Sprintf("seq_%02d", i), DataType: "integer",
			LastValue: int64(i * 1000), MinValue: 1, MaxValue: 2147483647,
			IncrementBy: 1, PctUsed: float64(i) / 8})
	}
	for i := 0; i < 60; i++ {
		st.queries = append(st.queries, st.newQuery())
	}
	st.fks = foreignKeys(s.Tables)
	for p := 0; p < 10; p++ {
		st.parts = append(st.parts, collector.PartitionInfo{ChildSchema: "app",
			ChildTable: fmt.Sprintf("events_p%02d", p), ParentSchema: "app",
			ParentTable: "events"})
	}
	st.system = collector.SystemStats{TotalBackends: 12, MaxConnections: 100,
		CacheHitRatio: 0.99, DBSizeBytes: 5 << 30, StatStatementsMax: 5000}
	st.config = newConfig()
	return st
}

// newConfig is the advisor's configuration snapshot: static settings and
// reloptions, moving connection states and WAL position.
func newConfig() collector.ConfigSnapshot {
	c := collector.ConfigSnapshot{ExtensionsAvailable: []string{"pg_stat_statements",
		"pgcrypto", "vector"}}
	for i := 0; i < 300; i++ {
		c.PGSettings = append(c.PGSettings, collector.PGSetting{
			Name: fmt.Sprintf("setting_%03d", i), Context: "user", Setting: fmt.Sprint(i * 7),
			Unit: "kB", Source: "default"})
	}
	for i := 0; i < 5; i++ {
		c.TableReloptions = append(c.TableReloptions, collector.TableReloption{
			SchemaName: "app", RelName: fmt.Sprintf("t%03d", i),
			Reloptions: "{autovacuum_vacuum_scale_factor=0.01}"})
	}
	for _, state := range []string{"active", "idle", "idle in transaction"} {
		c.ConnectionStates = append(c.ConnectionStates, collector.ConnectionState{State: state})
	}
	return c
}

func newTable(t int) collector.TableStats {
	return collector.TableStats{SchemaName: "app", RelName: fmt.Sprintf("t%03d", t),
		SeqScan: int64(t), NLiveTup: int64(1000 * (t + 1)), TotalBytes: int64(8192 * (t + 4)),
		TableBytes: int64(8192 * (t + 2)), IndexBytes: 16384, Relpersistence: "p",
		XIDAge: int64(1000 + t)}
}

func primaryKey(t int) collector.IndexStats {
	name := fmt.Sprintf("t%03d_pkey", t)
	return collector.IndexStats{SchemaName: "app", RelName: fmt.Sprintf("t%03d", t),
		IndexRelName: name, IndexBytes: 16384, IsUnique: true, IsPrimary: true,
		IsValid: true, IndexType: "btree",
		IndexDef: fmt.Sprintf("CREATE UNIQUE INDEX %s ON app.t%03d USING btree (id)", name, t)}
}

func secondaryIndex(t, i int) collector.IndexStats {
	name := fmt.Sprintf("ix_t%03d_c%05d", t, i)
	return collector.IndexStats{SchemaName: "app", RelName: fmt.Sprintf("t%03d", t),
		IndexRelName: name, IndexBytes: 8192, IsValid: true, IndexType: "btree",
		IndexDef: fmt.Sprintf("CREATE INDEX %s ON app.t%03d USING btree "+
			"(col_%05d, created_at DESC) WHERE (deleted_at IS NULL)", name, t, i)}
}

func foreignKeys(tables int) []collector.ForeignKey {
	var out []collector.ForeignKey
	for t := 1; t < tables && t <= 15; t++ {
		name := fmt.Sprintf("t%03d_parent_fkey", t)
		parent := fmt.Sprintf("t%03d", t-1)
		child := fmt.Sprintf("t%03d", t)
		out = append(out, collector.ForeignKey{TableName: child, ReferencedTable: parent,
			FKColumn: "parent_id", ConstraintName: name})
		if t%3 == 0 { // a two-column key: two rows for one constraint
			out = append(out, collector.ForeignKey{TableName: child,
				ReferencedTable: parent, FKColumn: "tenant_id", ConstraintName: name})
		}
	}
	return out
}

func (st *state) newQuery() collector.QueryStats {
	st.nextQID += 7919
	q := collector.QueryStats{QueryID: st.nextQID,
		Query: fmt.Sprintf("SELECT * FROM app.t%03d WHERE col_%05d = $1 AND created_at > $2",
			st.rng.Intn(st.sc.Tables), st.rng.Intn(100000)),
		Calls: int64(st.rng.Intn(1000) + 1), Rows: 10}
	q.TotalExecTime = float64(q.Calls) * (0.5 + st.rng.Float64())
	q.MeanExecTime = q.TotalExecTime / float64(q.Calls)
	return q
}

// advance applies cycle i's counter movement and scheduled events.
func (st *state) advance(i int) {
	st.moveCounters()
	ev := st.sc.Events
	if i == ev.CounterReset {
		st.resetCounters()
	}
	st.applyIndexDDL(i)
	if ev.QueryChurn > 0 && i%ev.QueryChurn == 0 && len(st.queries) > 1 {
		st.queries = append(st.queries[1:], st.newQuery())
	}
	st.system.DBSizeBytes += int64(st.rng.Intn(1 << 20))
	st.system.ActiveBackends = st.rng.Intn(10)
	st.system.TotalCheckpoints += int64(st.rng.Intn(2))
	st.system.BlkWriteTime += st.rng.Float64() * 10
	st.system.Deadlocks += int64(st.rng.Intn(2))
	st.wal += int64(st.rng.Intn(1 << 20))
	st.config.WALPosition = fmt.Sprintf("0/%X", st.wal)
	st.config.ConnectionChurn += st.rng.Intn(3)
	for k := range st.config.ConnectionStates {
		cs := &st.config.ConnectionStates[k]
		cs.Count = st.rng.Intn(10)
		cs.AvgDurationSeconds = float64(st.rng.Intn(1000)) / 10
	}
}

func (st *state) moveCounters() {
	for k := range st.tables {
		t := &st.tables[k]
		t.XIDAge += 37 // pg_sage's own writes advance the XID every cycle
		if st.rng.Intn(20) == 0 {
			n := int64(st.rng.Intn(500) + 1)
			t.NTupIns += n
			t.NLiveTup += n
			t.TotalBytes += 8192
		}
		if st.rng.Intn(100) == 0 {
			at := st.sc.Start.Add(time.Duration(k) * time.Minute).UTC()
			t.LastAutovacuum = &at
			t.AutovacuumCount++
		}
	}
	for k := range st.indexes {
		ix := &st.indexes[k]
		if !coldIndex(ix.IndexRelName) && st.rng.Intn(33) == 0 {
			n := int64(st.rng.Intn(10) + 1)
			ix.IdxScan += n
			ix.IdxTupRead += 3 * n
			ix.IdxTupFetch += 2 * n
		}
	}
	for k := range st.seqs {
		st.seqs[k].PctUsed += float64(k%7) * 0.05
		st.seqs[k].LastValue += int64(k % 7)
	}
	for k := range st.queries {
		q := &st.queries[k]
		n := int64(st.rng.Intn(50))
		q.Calls += n
		q.TotalExecTime += float64(n) * (0.2 + st.rng.Float64())
		if q.Calls > 0 {
			q.MeanExecTime = q.TotalExecTime / float64(q.Calls)
		}
	}
}

// coldIndex reports whether an index is never scanned: every tenth
// secondary index, so the unused-index evidence has subjects.
func coldIndex(name string) bool {
	return strings.HasPrefix(name, "ix_") && strings.HasSuffix(name, "5")
}

func (st *state) resetCounters() {
	for k := range st.indexes {
		ix := &st.indexes[k]
		ix.IdxScan, ix.IdxTupRead, ix.IdxTupFetch = 0, 0, 0
	}
	for k := range st.tables {
		t := &st.tables[k]
		t.SeqScan, t.SeqTupRead, t.IdxScan, t.IdxTupFetch = 0, 0, 0, 0
		t.NTupIns, t.NTupUpd, t.NTupDel, t.NTupHotUpd = 0, 0, 0, 0
	}
}

// applyIndexDDL drops, recreates, redefines and renames secondary indexes.
// Index k = s.Tables + n is the n-th secondary index.
func (st *state) applyIndexDDL(i int) {
	ev, base := st.sc.Events, st.sc.Tables
	if len(st.indexes) <= base+3 {
		return
	}
	switch i {
	case ev.DropIndex:
		victim := st.indexes[base]
		st.dropped = &victim
		st.indexes = append(st.indexes[:base:base], st.indexes[base+1:]...)
	case ev.DropIndex + 2:
		if st.dropped != nil {
			again := *st.dropped
			again.IdxScan, again.IdxTupRead, again.IdxTupFetch = 0, 0, 0
			again.IndexDef = fmt.Sprintf("CREATE INDEX %s ON app.%s USING btree "+
				"(tenant_id, created_at)", again.IndexRelName, again.RelName)
			st.indexes = append(st.indexes, again) // a new oid sorts last
			st.dropped = nil
		}
	}
	if i == ev.Redefine {
		ix := &st.indexes[base+1]
		ix.IndexType = "hash"
		ix.IndexDef = fmt.Sprintf("CREATE INDEX %s ON app.%s USING hash (email)",
			ix.IndexRelName, ix.RelName)
	}
	if i == ev.Rename {
		ix := &st.indexes[base+2]
		ix.IndexRelName += "_renamed"
		ix.IndexDef = fmt.Sprintf("CREATE INDEX %s ON app.%s USING btree (renamed_col)",
			ix.IndexRelName, ix.RelName)
	}
}

// cycle marshals the current state as the collector would persist it.
func (st *state) cycle(at time.Time, i int) (Cycle, error) {
	ev := st.sc.Events
	var indexes []collector.IndexStats // nil marshals as null, like the collector
	if i < ev.EmptyFrom || i >= ev.EmptyTo {
		indexes = st.indexes
	}
	sortSequences(st.seqs)
	sortQueries(st.queries)
	docs := map[string]any{
		"tables": st.tables, "indexes": indexes, "sequences": st.seqs,
		"queries": st.queries, "foreign_keys": st.fks, "partitions": st.parts,
		"system": st.system, "locks": []collector.LockInfo(nil),
		"config_data": &st.config,
	}
	if i == ev.Unavailable {
		delete(docs, "indexes")
	}
	c := Cycle{At: at}
	for cat, v := range docs {
		raw, err := json.Marshal(v)
		if err != nil {
			return Cycle{}, fmt.Errorf("marshal %s: %w", cat, err)
		}
		c.Docs = append(c.Docs, Doc{Category: cat, Data: raw})
	}
	sort.Slice(c.Docs, func(a, b int) bool { return c.Docs[a].Category < c.Docs[b].Category })
	return c, nil
}

// sortSequences orders like the collector: most used first.
func sortSequences(seqs []collector.SequenceStats) {
	sort.SliceStable(seqs, func(a, b int) bool {
		if seqs[a].PctUsed != seqs[b].PctUsed {
			return seqs[a].PctUsed > seqs[b].PctUsed
		}
		return seqs[a].SequenceName < seqs[b].SequenceName
	})
}

// sortQueries orders like the collector: most total time first.
func sortQueries(qs []collector.QueryStats) {
	sort.SliceStable(qs, func(a, b int) bool { return qs[a].TotalExecTime > qs[b].TotalExecTime })
}
