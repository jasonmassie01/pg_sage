package probes

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"
)

// M6 runway decoders: the XID runway, per-table freeze horizons, the
// holders of the xmin horizon, WAL position and directory, sequences
// against their binding limit and the sampled runway trends. Unknown
// numbers stay NaN; unavailable results are errors, never healthy.

func TestXIDRunway_Decodes(t *testing.T) {
	res := okResult(XIDRunwayProbe, nil, Row{"next_xid": int64(5_000_000),
		"mxid_counter": json.Number("4000"), "cluster_xid_age": int64(300_000),
		"cluster_mxid_age": int64(20), "database_xid_age": int64(250_000),
		"oldest_database": "orders", "freeze_max_age": int64(200_000_000),
		"multixact_freeze_max_age": int64(400_000_000), "autovacuum_on": true,
		"autovacuum_max_workers": int64(3), "autovacuum_workers": int64(3)})
	x, err := XIDRunwayOf(res)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if x.NextXID != 5e6 || x.MXIDCounter != 4000 || x.ClusterXIDAge != 3e5 ||
		x.ClusterMXIDAge != 20 || x.DatabaseXIDAge != 2.5e5 ||
		x.OldestDatabase != "orders" || x.FreezeMaxAge != 2e8 ||
		x.MXIDFreezeMaxAge != 4e8 || !x.AutovacuumOn || x.MaxWorkers != 3 ||
		x.Workers != 3 {
		t.Fatalf("xid runway = %+v", x)
	}
	if !x.WorkersSaturated() {
		t.Fatal("3 of 3 workers busy is saturated")
	}
	x.Workers = 2
	if x.WorkersSaturated() {
		t.Fatal("2 of 3 workers busy is not saturated")
	}
	x.MaxWorkers = math.NaN()
	if x.WorkersSaturated() {
		t.Fatal("an unknown worker maximum is never saturated")
	}
}

// Nil/empty: a missing counter is unknown, never zero; zero or two rows
// are unavailable.
func TestXIDRunway_UnknownAndUnavailable(t *testing.T) {
	x, err := XIDRunwayOf(okResult(XIDRunwayProbe, nil, Row{"next_xid": nil}))
	if err != nil || !math.IsNaN(x.NextXID) || !math.IsNaN(x.ClusterXIDAge) {
		t.Fatalf("unknown counters = %+v (%v), want NaN", x, err)
	}
	for _, res := range []Result{okResult(XIDRunwayProbe, nil),
		okResult(XIDRunwayProbe, nil, Row{}, Row{}),
		{ProbeID: XIDRunwayProbe, Status: StatusNoPrivilege, Reason: "denied"}} {
		var ue *UnavailableError
		if _, err := XIDRunwayOf(res); !errors.As(err, &ue) {
			t.Fatalf("%d rows / %s = %v, want UnavailableError", len(res.Rows),
				res.Status, err)
		}
	}
	if _, err := XIDRunwayOf(okResult(WALRunwayProbe, nil, Row{})); err == nil {
		t.Fatal("a wal_runway result decoded as xid_runway")
	}
}

func TestWraparoundTables_FractionUsesTheEffectiveMaximum(t *testing.T) {
	res := okResult(WraparoundTablesProbe, nil,
		Row{"relation": "public.events", "xid_age": int64(150_000),
			"mxid_age": int64(10), "freeze_max_age": int64(100_000),
			"mxid_freeze_max_age": int64(400_000_000), "autovacuum_enabled": false,
			"n_dead_tup": int64(7), "last_vacuum_age_s": 12.5,
			"autovacuum_count": int64(4), "vacuum_running": true},
		Row{"relation": "public.users", "xid_age": int64(1000),
			"mxid_age": int64(300_000_000), "freeze_max_age": int64(200_000_000),
			"mxid_freeze_max_age": int64(400_000_000), "autovacuum_enabled": true,
			"last_vacuum_age_s": nil})
	ts, err := WraparoundTables(res)
	if err != nil || len(ts) != 2 {
		t.Fatalf("tables = %+v (%v)", ts, err)
	}
	a := ts[0]
	if a.Relation != "public.events" || a.XIDAge != 150000 || a.FreezeMaxAge != 100000 ||
		a.AutovacuumEnabled || a.DeadTuples != 7 || a.LastVacuumAgeS != 12.5 ||
		a.AutovacuumCount != 4 || !a.VacuumRunning {
		t.Fatalf("table 0 = %+v", a)
	}
	if got := a.Fraction(); got != 1.5 {
		t.Fatalf("fraction = %v, want 1.5 (xid age over its effective maximum)", got)
	}
	if got := ts[1].Fraction(); got != 0.75 {
		t.Fatalf("multixact fraction = %v, want 0.75", got)
	}
	if !math.IsNaN(ts[1].LastVacuumAgeS) {
		t.Fatal("a table never vacuumed must have an unknown vacuum age")
	}
	// Boundary: a zero or unknown maximum never yields a fraction.
	if f := (WraparoundTable{XIDAge: 5, FreezeMaxAge: 0, MXIDAge: math.NaN(),
		MXIDFreezeMaxAge: math.NaN()}).Fraction(); !math.IsNaN(f) {
		t.Fatalf("fraction with no maximum = %v, want NaN", f)
	}
	if _, err := WraparoundTables(okResult(WraparoundTablesProbe, nil,
		Row{"xid_age": int64(1)})); err == nil {
		t.Fatal("a row without a relation decoded")
	}
	if ts, err := WraparoundTables(okResult(WraparoundTablesProbe, nil)); err != nil ||
		len(ts) != 0 {
		t.Fatalf("empty = %+v (%v), want no tables and no error", ts, err)
	}
}

func TestXminHolders_DecodeEveryKind(t *testing.T) {
	start := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	res := okResult(XminHorizon, nil,
		Row{"holder_kind": "session", "pid": int64(4242), "backend_start": start,
			"holder_name": nil, "state": "idle in transaction", "xmin_age": int64(90000),
			"xact_age_s": 600.5, "application_name": "billing", "active": nil},
		Row{"holder_kind": "prepared_xact", "pid": nil, "holder_name": "abc123",
			"state": "prepared", "xmin_age": int64(80000), "xact_age_s": 30.0},
		Row{"holder_kind": "slot_catalog", "holder_name": "cdc", "xmin_age": int64(10),
			"active": false})
	hs, err := XminHolders(res)
	if err != nil || len(hs) != 3 {
		t.Fatalf("holders = %+v (%v)", hs, err)
	}
	s := hs[0]
	if s.Kind != HolderSession || s.PID != 4242 || !s.BackendStart.Equal(start) ||
		s.State != "idle in transaction" || s.XminAge != 90000 || s.XactAgeS != 600.5 ||
		s.Application != "billing" {
		t.Fatalf("session holder = %+v", s)
	}
	if hs[1].Kind != HolderPreparedXact || hs[1].Name != "abc123" || hs[1].PID != 0 ||
		hs[1].XminAge != 80000 {
		t.Fatalf("prepared holder = %+v", hs[1])
	}
	if hs[2].Kind != HolderSlotCatalog || hs[2].Name != "cdc" || hs[2].Active {
		t.Fatalf("slot holder = %+v", hs[2])
	}
	if _, err := XminHolders(okResult(XminHorizon, nil, Row{"xmin_age": int64(1)})); err == nil {
		t.Fatal("a holder without a kind decoded")
	}
}

func TestWALRunwayAndDirectory_Decode(t *testing.T) {
	w, err := WALRunwayOf(okResult(WALRunwayProbe, nil, Row{
		"wal_position_bytes": 9.5e9, "max_wal_size_bytes": int64(1 << 30),
		"wal_keep_size_bytes": int64(0), "max_slot_wal_keep_size_bytes": int64(-1),
		"wal_segment_size_bytes": int64(16 << 20), "database_bytes": int64(5 << 30),
		"databases_unreadable": int64(0), "in_recovery": false}))
	if err != nil {
		t.Fatalf("wal runway: %v", err)
	}
	if w.PositionBytes != 9.5e9 || w.MaxWALSize != 1<<30 || w.MaxSlotWALKeepSize != -1 ||
		w.SegmentSize != 16<<20 || w.DatabaseBytes != 5<<30 || w.UnreadableDatabases != 0 ||
		w.InRecovery {
		t.Fatalf("wal runway = %+v", w)
	}
	if w.SlotKeepBounded() {
		t.Fatal("max_slot_wal_keep_size -1 is unbounded")
	}
	w.MaxSlotWALKeepSize = 0
	if !w.SlotKeepBounded() {
		t.Fatal("max_slot_wal_keep_size 0 bounds slots (to nothing retained)")
	}
	d, err := WALDirectoryOf(okResult(WALDirectoryProbe, nil, Row{
		"wal_dir_bytes": int64(48 << 20), "wal_files": int64(3),
		"archive_ready_files": int64(2)}))
	if err != nil || d.Bytes != 48<<20 || d.Files != 3 || d.ReadyFiles != 2 {
		t.Fatalf("wal directory = %+v (%v)", d, err)
	}
	denied := Result{ProbeID: WALDirectoryProbe, Status: StatusNoPrivilege,
		Reason: "insufficient_privilege"}
	var ue *UnavailableError
	if _, err := WALDirectoryOf(denied); !errors.As(err, &ue) ||
		ue.Status != StatusNoPrivilege {
		t.Fatalf("denied wal directory = %v, want UnavailableError(no_privilege)", err)
	}
}

func TestSequences_BindingLimit(t *testing.T) {
	row := func(seq, typ string, max float64, owner string, ownerMax any) Row {
		return Row{"sequence": seq, "data_type": typ, "increment_by": int64(1),
			"cycle": false, "last_value": int64(1000), "min_value": int64(1),
			"max_value": max, "type_max": typeMaxOf(typ), "owner_column": owner,
			"owner_type_max": ownerMax, "effective_limit": max,
			"fraction_used": 0.9}
	}
	res := okResult(SequenceRunwayProbe, nil,
		row("public.a_seq", "integer", 2147483647, "public.a.id", int64(2147483647)),
		row("public.b_seq", "bigint", 9223372036854775807, "public.b.id",
			int64(2147483647)),
		row("public.c_seq", "bigint", 1_000_000, "public.c.id",
			json.Number("9223372036854775807")),
		row("public.d_seq", "bigint", 9223372036854775807, "", nil))
	ss, err := Sequences(res)
	if err != nil || len(ss) != 4 {
		t.Fatalf("sequences = %+v (%v)", ss, err)
	}
	want := []string{LimitSequenceType, LimitColumnType, LimitExplicitMax,
		LimitSequenceType}
	for i, s := range ss {
		if got := s.BindingLimit(); got != want[i] {
			t.Errorf("%s binding limit = %q, want %q", s.Sequence, got, want[i])
		}
	}
	if ss[0].Fraction != 0.9 || ss[0].LastValue != 1000 || ss[0].Increment != 1 ||
		ss[0].OwnerColumn != "public.a.id" {
		t.Fatalf("sequence 0 = %+v", ss[0])
	}
	if !math.IsNaN(ss[3].OwnerTypeMax) {
		t.Fatal("a sequence without an owner has an unknown owner maximum")
	}
	never := okResult(SequenceRunwayProbe, nil, Row{"sequence": "public.e_seq",
		"data_type": "bigint", "last_value": nil, "max_value": int64(100),
		"type_max": int64(9223372036854775807)})
	es, err := Sequences(never)
	if err != nil || !math.IsNaN(es[0].LastValue) || !math.IsNaN(es[0].Fraction) {
		t.Fatalf("never-called sequence = %+v (%v), want unknown value", es, err)
	}
	if _, err := Sequences(okResult(SequenceRunwayProbe, nil,
		Row{"data_type": "bigint"})); err == nil {
		t.Fatal("a row without a sequence name decoded")
	}
}

func typeMaxOf(typ string) any {
	switch typ {
	case "integer":
		return int64(2147483647)
	case "smallint":
		return int64(32767)
	}
	return int64(9223372036854775807)
}

func TestRunwayTrends_Projection(t *testing.T) {
	first := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)
	last := first.Add(time.Hour)
	res := okResult(RunwayTrendsProbe, nil,
		Row{"kind": "sequence", "subject": "public.a_seq", "samples": int64(13),
			"first_at": first, "last_at": last, "last_value": 1000.0,
			"last_limit": 4600.0, "rate_per_s": 1.0, "r2": 0.99},
		Row{"kind": "wal_position", "subject": "cluster", "samples": int64(2),
			"first_at": first, "last_at": last, "last_value": 5.0,
			"last_limit": nil, "rate_per_s": 2.0, "r2": nil})
	ts, err := RunwayTrends(res)
	if err != nil || len(ts) != 2 {
		t.Fatalf("trends = %+v (%v)", ts, err)
	}
	a := ts[0]
	if a.Kind != RunwaySequence || a.Subject != "public.a_seq" || a.Samples != 13 ||
		a.SpanS() != 3600 || a.LastValue != 1000 || a.Limit != 4600 || a.RatePerS != 1 ||
		a.R2 != 0.99 {
		t.Fatalf("trend 0 = %+v", a)
	}
	if got := a.SecondsToLimit(); got != 3600 {
		t.Fatalf("seconds to limit = %v, want 3600", got)
	}
	if got := ts[1].SecondsToLimit(); !math.IsNaN(got) {
		t.Fatalf("no limit: seconds = %v, want NaN (unknown)", got)
	}
	if !math.IsNaN(ts[1].R2) {
		t.Fatal("an unknown r2 must stay NaN")
	}
	if tr, ok := FindTrend(ts, RunwaySequence, "public.a_seq"); !ok || tr.Samples != 13 {
		t.Fatalf("find = %+v %v", tr, ok)
	}
	if _, ok := FindTrend(ts, RunwaySequence, "public.b_seq"); ok {
		t.Fatal("found a trend that does not exist")
	}
}

// Boundaries: already at the limit is 0; a flat or falling series never
// reaches it (+Inf); an unknown rate is unknown (NaN).
func TestRunwayTrend_ProjectionBoundaries(t *testing.T) {
	cases := []struct {
		name  string
		tr    RunwayTrend
		check func(float64) bool
	}{
		{"at limit", RunwayTrend{LastValue: 10, Limit: 10, RatePerS: 1},
			func(s float64) bool { return s == 0 }},
		{"over limit", RunwayTrend{LastValue: 11, Limit: 10, RatePerS: 0},
			func(s float64) bool { return s == 0 }},
		{"flat", RunwayTrend{LastValue: 1, Limit: 10, RatePerS: 0},
			func(s float64) bool { return math.IsInf(s, 1) }},
		{"falling", RunwayTrend{LastValue: 1, Limit: 10, RatePerS: -3},
			func(s float64) bool { return math.IsInf(s, 1) }},
		{"unknown rate", RunwayTrend{LastValue: 1, Limit: 10, RatePerS: math.NaN()},
			math.IsNaN},
		{"unknown value", RunwayTrend{LastValue: math.NaN(), Limit: 10, RatePerS: 1},
			math.IsNaN},
	}
	for _, c := range cases {
		if got := c.tr.SecondsToLimit(); !c.check(got) {
			t.Errorf("%s: seconds to limit = %v", c.name, got)
		}
	}
}

func TestRunwayTrends_RejectsRowsWithoutIdentity(t *testing.T) {
	if _, err := RunwayTrends(okResult(RunwayTrendsProbe, nil,
		Row{"kind": "xid", "samples": int64(2)})); err == nil {
		t.Fatal("a trend without a subject decoded")
	}
	if _, err := RunwayTrends(okResult(RunwayTrendsProbe, nil,
		Row{"kind": "xid", "subject": "cluster"})); err == nil {
		t.Fatal("a trend without a sample count decoded")
	}
	unsupported := Result{ProbeID: RunwayTrendsProbe, Status: StatusUnsupported,
		Reason: "undefined_table"}
	var ue *UnavailableError
	if _, err := RunwayTrends(unsupported); !errors.As(err, &ue) {
		t.Fatalf("unsupported trends = %v, want UnavailableError", err)
	}
}

func TestAutovacuumCancellations_Decodes(t *testing.T) {
	n, err := AutovacuumCancellationCount(okResult(AutovacuumCancellations, nil,
		Row{"cancel_incidents": int64(3)}))
	if err != nil || n != 3 {
		t.Fatalf("cancellations = %v (%v)", n, err)
	}
	if n, err := AutovacuumCancellationCount(okResult(AutovacuumCancellations, nil,
		Row{"cancel_incidents": nil})); err == nil {
		t.Fatalf("an unknown count decoded as %v", n)
	}
	if _, err := AutovacuumCancellationCount(okResult(AutovacuumCancellations, nil)); err == nil {
		t.Fatal("an empty cancellation result decoded")
	}
}
