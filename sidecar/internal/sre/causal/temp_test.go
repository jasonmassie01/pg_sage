package causal

import (
	"math"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Temp-file explosion family: one runaway query holding a large live
// spill, one statement that spills on every call, or a workload whose
// many statements each spill a little (work_mem too small for them).
// Live temp files come from temp_file_holders, finished spills from
// pg_stat_statements (temp_spill_statements) and pg_stat_database.

func nanValue() float64 { return math.NaN() }

var tempReset = m6t0.Add(-48 * time.Hour)

func tempActivity(ev string, at time.Duration, files, bytes float64) Observation {
	return obsAt(ev, probes.TempFileActivity, at, probes.Row{"temp_files": files,
		"temp_bytes": bytes, "stats_reset": tempReset, "work_mem_bytes": int64(4 * mib),
		"hash_mem_multiplier": 2.0, "temp_file_limit_kb": int64(-1),
		"temp_tablespaces_set": false, "server_started_at": tempReset})
}

type holder struct {
	pid       int64
	bytes     float64
	currentDB bool
}

func holders(ev string, at time.Duration, hs ...holder) Observation {
	var total float64
	for _, h := range hs {
		total += h.bytes
	}
	var rows []probes.Row
	for _, h := range hs {
		rows = append(rows, probes.Row{"pid": h.pid, "backend_start": tempReset,
			"in_current_database": h.currentDB, "state": "active", "query_id": int64(555),
			"query_age_s": 40.0, "files": int64(2), "bytes": h.bytes,
			"total_bytes": total})
	}
	return obsAt(ev, probes.TempFileHolders, at, rows...)
}

type spill struct {
	qid         int64
	calls, blks int64
}

func spills(ev string, at time.Duration, ss ...spill) Observation {
	var rows []probes.Row
	for _, s := range ss {
		rows = append(rows, probes.Row{"queryid": s.qid, "calls": s.calls,
			"temp_blks_written": s.blks, "temp_blks_read": s.blks,
			"total_exec_ms": 100.0, "block_size": int64(8192), "stats_reset": tempReset})
	}
	return obsAt(ev, probes.TempSpillStatements, at, rows...)
}

const second = 9 * time.Second

func TestTemp_RunawayQueryHoldsALargeLiveSpill(t *testing.T) {
	d := DiagnoseTempFiles([]Observation{
		tempActivity("A1", 0, 100, 10<<30), holders("H1", 0, holder{77, 150 * mib, true}),
		spills("S1", 0, spill{111, 5, 100}),
		tempActivity("A2", second, 100, 10<<30),
		holders("H2", second, holder{77, 200 * mib, true}, holder{78, 1 * mib, true}),
		spills("S2", second, spill{111, 5, 100})})
	if d.Family != FamilyTempFiles {
		t.Fatalf("family = %s", d.Family)
	}
	wantRoot(t, d, RunawaySpillQuery)
	if d.Root.Subject != "pid 77" {
		t.Fatalf("subject = %q, want pid 77", d.Root.Subject)
	}
	wantFact(t, d.Root.Support, "H2", "209715200 bytes")
	wantFact(t, d.Root.Support, "H2", "grew")
	wantStatus(t, d, RepeatedSpillStatement, StatusRuledOut)
	wantStatus(t, d, WorkMemUndersized, StatusRuledOut)
	everyHypothesisCarriesRefutation(t, d)
}

func TestTemp_RepeatedSpillingStatement(t *testing.T) {
	d := DiagnoseTempFiles([]Observation{
		tempActivity("A1", 0, 100, 1<<30), holders("H1", 0, holder{88, 20 * mib, true}),
		spills("S1", 0, spill{111, 10, 1000}, spill{222, 3, 50}),
		tempActivity("A2", second, 120, 1<<30+650*mib),
		holders("H2", second, holder{88, 30 * mib, true}),
		spills("S2", second, spill{111, 30, 81000}, spill{222, 3, 50})})
	wantRoot(t, d, RepeatedSpillStatement)
	if d.Root.Subject != "queryid 111" {
		t.Fatalf("subject = %q", d.Root.Subject)
	}
	wantFact(t, d.Root.Support, "S2", "20 calls")
	wantStatus(t, d, RunawaySpillQuery, StatusAlternative)
	wantStatus(t, d, WorkMemUndersized, StatusRuledOut)
}

func TestTemp_ManyStatementsSpillingALittle(t *testing.T) {
	d := DiagnoseTempFiles([]Observation{
		tempActivity("A1", 0, 100, 1<<30), holders("H1", 0),
		spills("S1", 0, spill{1, 10, 100}, spill{2, 10, 100}, spill{3, 10, 100},
			spill{4, 10, 100}),
		tempActivity("A2", second, 180, 1<<30+64*mib), holders("H2", second),
		spills("S2", second, spill{1, 30, 2100}, spill{2, 30, 2100}, spill{3, 30, 2100},
			spill{4, 30, 2100})})
	wantRoot(t, d, WorkMemUndersized)
	wantFact(t, d.Root.Support, "S2", "4 statements")
	wantStatus(t, d, RunawaySpillQuery, StatusRuledOut)
	wantStatus(t, d, RepeatedSpillStatement, StatusAlternative)
}

// Decoy: a large cumulative counter from an earlier spill, nothing now.
func TestTemp_HistoricCounterIsInconclusive(t *testing.T) {
	d := DiagnoseTempFiles([]Observation{
		tempActivity("A1", 0, 5000, 900<<30), holders("H1", 0),
		spills("S1", 0, spill{111, 900, 9000000}),
		tempActivity("A2", second, 5000, 900<<30), holders("H2", second),
		spills("S2", second, spill{111, 900, 9000000})})
	wantInconclusive(t, d)
	for _, id := range []NodeID{RunawaySpillQuery, RepeatedSpillStatement,
		WorkMemUndersized} {
		wantStatus(t, d, id, StatusRuledOut)
	}
}

// Decoy: a live spill below the runaway floor.
func TestTemp_SmallLiveSpillIsInconclusive(t *testing.T) {
	d := DiagnoseTempFiles([]Observation{
		tempActivity("A1", 0, 10, mib), holders("H1", 0, holder{90, 16 * mib, true}),
		spills("S1", 0), tempActivity("A2", second, 10, mib),
		holders("H2", second, holder{90, 16 * mib, true}), spills("S2", second)})
	wantInconclusive(t, d)
	wantStatus(t, d, RunawaySpillQuery, StatusAlternative)
}

func TestTemp_RunawayFloorBoundary(t *testing.T) {
	at := func(b float64) Diagnosis {
		return DiagnoseTempFiles([]Observation{tempActivity("A1", 0, 1, 1),
			holders("H1", 0, holder{91, b, true}), spills("S1", 0),
			tempActivity("A2", second, 1, 1), holders("H2", second, holder{91, b, true}),
			spills("S2", second)})
	}
	if d := at(64 * mib); statusOf(d, RunawaySpillQuery) != StatusRoot {
		t.Fatalf("64 MiB live: %s", statusOf(d, RunawaySpillQuery))
	}
	if d := at(64*mib - 1); statusOf(d, RunawaySpillQuery) != StatusAlternative {
		t.Fatalf("64 MiB - 1 live: %s", statusOf(d, RunawaySpillQuery))
	}
}

func TestTemp_OtherDatabasesHoldersAreNotThisDatabases(t *testing.T) {
	d := DiagnoseTempFiles([]Observation{tempActivity("A1", 0, 1, 1),
		holders("H1", 0, holder{92, 900 * mib, false}), spills("S1", 0),
		tempActivity("A2", second, 1, 1), holders("H2", second, holder{92, 900 * mib, false}),
		spills("S2", second)})
	wantInconclusive(t, d)
	h, _ := hypothesisOf(d, RunawaySpillQuery)
	if h.Status != StatusRuledOut {
		t.Fatalf("runaway = %+v, want ruled out: the holder is another database's", h)
	}
}

// Without pg_stat_statements the finished-spill hypotheses cannot be
// scored while temp files are being written; a live runaway still can.
// (With no temp file written at all, pg_stat_database alone rules them
// out.)
func TestTemp_WithoutPgStatStatements(t *testing.T) {
	noExt := func(ev string) Observation {
		return failedObs(ev, probes.TempSpillStatements, probes.StatusUnsupported,
			"extension_not_installed")
	}
	d := DiagnoseTempFiles([]Observation{tempActivity("A1", 0, 1, 1),
		holders("H1", 0, holder{93, 300 * mib, true}), noExt("S1"),
		tempActivity("A2", second, 6, 1+50*mib),
		holders("H2", second, holder{93, 300 * mib, true}),
		noExt("S2")})
	wantRoot(t, d, RunawaySpillQuery)
	wantMissing(t, d, probes.TempSpillStatements, "extension_not_installed")
	for _, id := range []NodeID{RepeatedSpillStatement, WorkMemUndersized} {
		wantStatus(t, d, id, StatusAlternative)
	}
}

func TestTemp_HoldersWithoutPrivilege(t *testing.T) {
	denied := func(ev string) Observation {
		return failedObs(ev, probes.TempFileHolders, probes.StatusNoPrivilege,
			"insufficient_privilege")
	}
	d := DiagnoseTempFiles([]Observation{tempActivity("A1", 0, 1, 1), denied("H1"),
		spills("S1", 0), tempActivity("A2", second, 1, 1), denied("H2"), spills("S2", second)})
	wantInconclusive(t, d)
	wantMissing(t, d, probes.TempFileHolders, "insufficient_privilege")
	wantStatus(t, d, RunawaySpillQuery, StatusAlternative)
}

func TestTemp_StatementStatsResetMakesDeltasUnknown(t *testing.T) {
	reset := spills("S2", second, spill{111, 2, 50})
	reset.Result.Rows[0]["stats_reset"] = m6t0
	d := DiagnoseTempFiles([]Observation{tempActivity("A1", 0, 1, 1), holders("H1", 0),
		spills("S1", 0, spill{111, 900, 90000}), tempActivity("A2", second, 3, 1+mib),
		holders("H2", second), reset})
	wantMissing(t, d, probes.TempSpillStatements, "counter_reset")
	wantStatus(t, d, RepeatedSpillStatement, StatusAlternative)
}

// Spills far larger than work_mem are not a work_mem problem.
func TestTemp_HugeSpillsRuleOutWorkMem(t *testing.T) {
	d := DiagnoseTempFiles([]Observation{
		tempActivity("A1", 0, 100, 1<<30), holders("H1", 0),
		spills("S1", 0, spill{1, 1, 1}, spill{2, 1, 1}, spill{3, 1, 1}),
		tempActivity("A2", second, 103, 1<<30+900*mib), holders("H2", second),
		spills("S2", second, spill{1, 2, 38401}, spill{2, 2, 38401}, spill{3, 2, 38401})})
	h, _ := hypothesisOf(d, WorkMemUndersized)
	if h.Status != StatusRuledOut {
		t.Fatalf("work_mem = %+v, want ruled out by spills over 64 times work_mem", h)
	}
}

func TestTemp_UnlimitedTempFileLimitIsObserved(t *testing.T) {
	d := DiagnoseTempFiles([]Observation{tempActivity("A1", 0, 1, 1), holders("H1", 0),
		spills("S1", 0), tempActivity("A2", second, 1, 1), holders("H2", second),
		spills("S2", second)})
	wantFact(t, d.Observed, "A2", "temp_file_limit")
}

func TestTemp_NothingCollected(t *testing.T) {
	d := DiagnoseTempFiles(nil)
	wantInconclusive(t, d)
	wantMissing(t, d, probes.TempFileActivity, "not_collected")
	wantMissing(t, d, probes.TempFileHolders, "not_collected")
}
