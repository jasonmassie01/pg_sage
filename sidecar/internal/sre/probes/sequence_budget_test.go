package probes

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Background sampling budget (dogfood lifeos-1): a spec may declare a
// background statement budget, used only by RunBackground (the runway
// monitor's slow-cadence sampling), bounded by its own ceiling and never
// below the spec's incident-time budget. Only sequence_runway has one.

func TestNewRegistry_BackgroundTimeoutBounds(t *testing.T) {
	valid := map[string]time.Duration{
		"none":           0,
		"equal to stmt":  200 * time.Millisecond,
		"at the ceiling": MaxBackgroundStatementTimeout,
	}
	for name, d := range valid {
		t.Run(name, func(t *testing.T) {
			s := validSpec()
			s.BackgroundTimeout = d
			if _, err := NewRegistry(s); err != nil {
				t.Fatalf("background timeout %s rejected: %v", d, err)
			}
		})
	}
	invalid := map[string]time.Duration{
		"negative":         -time.Millisecond,
		"below statement":  100 * time.Millisecond,
		"above ceiling":    MaxBackgroundStatementTimeout + time.Millisecond,
		"far above":        time.Minute,
		"one nanosec over": MaxBackgroundStatementTimeout + 1,
	}
	for name, d := range invalid {
		t.Run(name, func(t *testing.T) {
			s := validSpec()
			s.BackgroundTimeout = d
			_, err := NewRegistry(s)
			if err == nil || !strings.Contains(err.Error(), "background") {
				t.Fatalf("background timeout %s: err = %v, want a background "+
					"budget error", d, err)
			}
		})
	}
}

// Background budgets are for the runway monitor's sampling only:
// sequence_runway (slow cadence) and wraparound_tables (per-table
// statistics, which on PostgreSQL 14 wait for the stats collector).
func TestCatalog_OnlyRunwaySamplingProbesHaveABackgroundBudget(t *testing.T) {
	if MaxBackgroundStatementTimeout <= MaxStatementTimeout {
		t.Fatalf("background ceiling %s must exceed the incident ceiling %s",
			MaxBackgroundStatementTimeout, MaxStatementTimeout)
	}
	sampled := map[ID]bool{SequenceRunwayProbe: true, WraparoundTablesProbe: true}
	for _, id := range Catalog().IDs() {
		spec, _ := Catalog().Spec(id)
		switch {
		case sampled[id]:
			if spec.BackgroundTimeout != MaxBackgroundStatementTimeout ||
				spec.StatementTimeout != MaxStatementTimeout {
				t.Errorf("%s budgets = %s / %s, want %s / %s", id,
					spec.StatementTimeout, spec.BackgroundTimeout,
					MaxStatementTimeout, MaxBackgroundStatementTimeout)
			}
		case spec.BackgroundTimeout != 0:
			t.Errorf("%s declares a background budget %s", id, spec.BackgroundTimeout)
		}
	}
}

// The v3 query reads last_value directly (no join of pg_sequences by
// name), bounds the sequences one statement reads (v1.8.3: a fixed cap,
// larger catalogs are read in slices), reports its coverage and slices by
// a hash of the sequence's OID.
func TestCatalog_SequenceRunwayV3IsBounded(t *testing.T) {
	spec, ok := Catalog().Spec(SequenceRunwayProbe)
	if !ok || spec.Version != "v3" || spec.MaxRows != 50 || spec.Args != ArgsSlice {
		t.Fatalf("sequence_runway spec = %+v", spec)
	}
	sql := spec.Variants[0].SQL
	for _, want := range []string{"pg_sequence_last_value", "LIMIT LEAST(" +
		strconv.Itoa(SequenceScanCap), "max_locks_per_transaction", "sequences_total", "sequences_scanned",
		"sequences_used", "sequences_unreadable", "pg_is_other_temp_schema",
		"coverage_only", "hashint8", "sre:sequence_runway v3"} {
		if !strings.Contains(sql, want) {
			t.Errorf("sequence_runway SQL lacks %q", want)
		}
	}
	if strings.Contains(sql, "pg_sequences") {
		t.Error("sequence_runway still joins the pg_sequences view")
	}
	// One statement must stay inside the 500 ms incident budget on a busy
	// server (each read opens and locks a sequence); lifeos (12,038
	// sequences) is read in slices.
	if SequenceScanCap < 1000 || SequenceScanCap > 5000 {
		t.Fatalf("scan cap %d: one statement's reads and locks must stay bounded",
			SequenceScanCap)
	}
}

func coverageRow(seq string, frac float64) Row {
	return Row{"sequence": seq, "data_type": "integer", "last_value": int64(10),
		"min_value": int64(1), "max_value": int64(2147483647),
		"effective_limit": int64(2147483647), "fraction_used": frac,
		"sequences_total": int64(12038), "sequences_scanned": int64(12038),
		"sequences_used": int64(1146), "sequences_unreadable": int64(3)}
}

func TestSequenceCoverageOf_Decodes(t *testing.T) {
	res := okResult(SequenceRunwayProbe, nil, coverageRow("public.a_seq", 0.5),
		coverageRow("public.b_seq", 0.4))
	res.Truncated = true
	c, err := SequenceCoverageOf(res)
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	want := SequenceCoverage{Total: 12038, Scanned: 12038, Used: 1146, Unreadable: 3,
		Reported: 2, Truncated: true}
	if c != want {
		t.Fatalf("coverage = %+v, want %+v", c, want)
	}
	if c.ScanCapped() {
		t.Fatal("every sequence was scanned, yet the scan is reported capped")
	}
	capped := coverageRow("public.a_seq", 0.5)
	capped["sequences_total"] = int64(30000)
	capped["sequences_scanned"] = int64(SequenceScanCap)
	c, err = SequenceCoverageOf(okResult(SequenceRunwayProbe, nil, capped))
	if err != nil || !c.ScanCapped() || c.Truncated || c.Reported != 1 {
		t.Fatalf("capped coverage = %+v (%v)", c, err)
	}
}

// Nil/empty: no used sequence among those read leaves no rows; the
// coverage is then zero, not an error. A failed probe is an error, never
// an empty coverage.
func TestSequenceCoverageOf_EmptyAndFailed(t *testing.T) {
	c, err := SequenceCoverageOf(okResult(SequenceRunwayProbe, nil))
	if err != nil || c != (SequenceCoverage{}) {
		t.Fatalf("empty coverage = %+v (%v), want zero", c, err)
	}
	failed := Result{ProbeID: SequenceRunwayProbe, Status: StatusError,
		Reason: "statement_timeout"}
	if _, err := SequenceCoverageOf(failed); err == nil ||
		!strings.Contains(err.Error(), "statement_timeout") {
		t.Fatalf("failed probe coverage err = %v, want the timeout named", err)
	}
	wrong := okResult(XIDRunwayProbe, nil, coverageRow("public.a_seq", 0.5))
	if _, err := SequenceCoverageOf(wrong); err == nil {
		t.Fatal("coverage decoded from another probe's result")
	}
}

// Invalid input: a row without the coverage counts (an older probe
// version) is rejected rather than read as zero sequences.
func TestSequenceCoverageOf_RejectsMissingCounts(t *testing.T) {
	for _, col := range []string{"sequences_total", "sequences_scanned",
		"sequences_used", "sequences_unreadable"} {
		row := coverageRow("public.a_seq", 0.5)
		delete(row, col)
		if _, err := SequenceCoverageOf(okResult(SequenceRunwayProbe, nil,
			row)); err == nil || !strings.Contains(err.Error(), col) {
			t.Errorf("row without %s: err = %v, want it named", col, err)
		}
	}
	row := coverageRow("public.a_seq", 0.5)
	row["sequences_scanned"] = int64(20000) // more scanned than exist
	if _, err := SequenceCoverageOf(okResult(SequenceRunwayProbe, nil,
		row)); err == nil {
		t.Fatal("coverage with more scanned than total sequences decoded")
	}
}
