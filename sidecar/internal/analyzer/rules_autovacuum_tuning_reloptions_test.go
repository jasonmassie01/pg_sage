package analyzer

import (
	"strconv"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// rules-02: the rule reads each table's own reloptions (collected with
// its stats), so a DBA's tuning holds without the advisor's ConfigData.

// avChurning is a 2M-row table that churns past its live count: the
// 0.05 tier, and a finding unless its own reloptions say otherwise.
func avChurning(opts []string) collector.TableStats {
	t := avTuneTable("public", "events", 2_000_000, 3_000_000, 0)
	t.Reloptions = opts
	return t
}

func avFindings(t *testing.T, snap *collector.Snapshot) []Finding {
	t.Helper()
	cfg := testConfig()
	cfg.Analyzer.AutovacuumTuneMinRows = 1_000_000
	return ruleAutovacuumTuning(snap, nil, cfg, nil)
}

// A DBA's tighter per-table setting holds with or without ConfigData
// (a runtime without an LLM never collects ConfigData).
func TestRuleAutovacuumTuning_DBATunedTableNeedsNoConfigSnapshot(t *testing.T) {
	opts := []string{"autovacuum_vacuum_scale_factor=0.01"}
	without := &collector.Snapshot{Tables: []collector.TableStats{avChurning(opts)}}
	if got := avFindings(t, without); len(got) != 0 {
		t.Fatalf("table tuned to 0.01 re-recommended without ConfigData: %s",
			got[0].RecommendedSQL)
	}
	with := &collector.Snapshot{
		Tables: []collector.TableStats{avChurning(opts)},
		ConfigData: &collector.ConfigSnapshot{TableReloptions: []collector.TableReloption{{
			SchemaName: "public", RelName: "events",
			Reloptions: "{autovacuum_vacuum_scale_factor=0.01}"}}},
	}
	if got := avFindings(t, with); len(got) != 0 {
		t.Fatalf("table tuned to 0.01 re-recommended with ConfigData: %s",
			got[0].RecommendedSQL)
	}
}

// The table's own reloptions are the source of truth: a stale ConfigData
// row naming a looser value does not override a tighter current setting.
func TestRuleAutovacuumTuning_TableReloptionsWinOverConfigData(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			avChurning([]string{"autovacuum_vacuum_scale_factor=0.01"})},
		ConfigData: &collector.ConfigSnapshot{TableReloptions: []collector.TableReloption{{
			SchemaName: "public", RelName: "events",
			Reloptions: "{autovacuum_vacuum_scale_factor=0.2}"}}},
	}
	if got := avFindings(t, snap); len(got) != 0 {
		t.Fatalf("stale ConfigData loosened a 0.01 table: %s", got[0].RecommendedSQL)
	}
}

func TestRuleAutovacuumTuning_ExplicitSettingBoundaries(t *testing.T) {
	cases := []struct {
		name  string
		live  int64
		value string
		want  string // "" = no finding, else the recommended scale factor
	}{
		{"equal to the 0.05 target", 2_000_000, "0.05", ""},
		{"just above the 0.05 target", 2_000_000, "0.0500001", "0.05"},
		{"looser than the 0.05 target", 2_000_000, "0.10", "0.05"},
		{"default-like 0.2 explicit", 2_000_000, "0.2", "0.05"},
		{"zero (vacuum on every change)", 2_000_000, "0", ""},
		{"equal to the 0.02 tier", 10_000_000, "0.02", ""},
		{"looser than the 0.02 tier", 10_000_000, "0.03", "0.02"},
		{"equal to the 0.01 tier", 50_000_000, "0.01", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tbl := avTuneTable("public", "events", c.live, c.live+1, 0)
			tbl.Reloptions = []string{"fillfactor=90",
				"autovacuum_vacuum_scale_factor=" + c.value}
			got := avFindings(t, &collector.Snapshot{Tables: []collector.TableStats{tbl}})
			if c.want == "" {
				if len(got) != 0 {
					t.Fatalf("explicit %s loosened or re-applied: %s", c.value,
						got[0].RecommendedSQL)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("explicit %s: %d findings, want 1", c.value, len(got))
			}
			wantSQL := "autovacuum_vacuum_scale_factor = " + c.want + ");"
			if !strings.HasSuffix(got[0].RecommendedSQL, wantSQL) {
				t.Errorf("RecommendedSQL = %q, want suffix %q",
					got[0].RecommendedSQL, wantSQL)
			}
		})
	}
}

// A finding that tightens an explicit DBA setting rolls back to that
// setting, not to the global default.
func TestRuleAutovacuumTuning_RollbackRestoresExplicitSetting(t *testing.T) {
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		avChurning([]string{"autovacuum_vacuum_scale_factor=0.10"})}}
	got := avFindings(t, snap)
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1", len(got))
	}
	// vacuum-07: the stored text is restored verbatim (0.10, not 0.1), so
	// pg_class.reloptions after the rollback equals the array before.
	want := `ALTER TABLE "public"."events" SET (autovacuum_vacuum_scale_factor = 0.10);`
	if got[0].RollbackSQL != want {
		t.Errorf("RollbackSQL = %q, want %q", got[0].RollbackSQL, want)
	}
	if sf, ok := got[0].Detail["current_scale_factor"].(float64); !ok || sf != 0.10 {
		t.Errorf("Detail current_scale_factor = %v, want 0.10",
			got[0].Detail["current_scale_factor"])
	}
}

// Unparseable reloptions mean the DBA's setting is unknown: fail closed,
// skip the table, never panic.
func TestRuleAutovacuumTuning_MalformedReloptionsSkipTable(t *testing.T) {
	cases := [][]string{
		{"autovacuum_vacuum_scale_factor=abc"},
		{"=,"},
		{"autovacuum_vacuum_scale_factor="},
		{"autovacuum_vacuum_scale_factor"},
		{"autovacuum_vacuum_scale_factor=NaN"},
		{"autovacuum_vacuum_scale_factor=Inf"},
		{"autovacuum_vacuum_scale_factor=-0.5"},
		{"autovacuum_enabled=maybe"},
		{"autovacuum_enabled=o"}, // PostgreSQL rejects "o" as ambiguous
		{""},
	}
	for _, opts := range cases {
		snap := &collector.Snapshot{Tables: []collector.TableStats{avChurning(opts)}}
		if got := avFindings(t, snap); len(got) != 0 {
			t.Errorf("reloptions %q: %d findings, want 0 (unknown setting)", opts, len(got))
		}
	}
}

// No reloptions is the normal untuned state: the global default applies.
func TestRuleAutovacuumTuning_NoReloptionsUsesDefault(t *testing.T) {
	for _, opts := range [][]string{nil, {}, {"fillfactor=90"}} {
		snap := &collector.Snapshot{Tables: []collector.TableStats{avChurning(opts)}}
		got := avFindings(t, snap)
		if len(got) != 1 {
			t.Fatalf("reloptions %q: %d findings, want 1", opts, len(got))
		}
		if !strings.HasSuffix(got[0].RecommendedSQL, "scale_factor = 0.05);") {
			t.Errorf("reloptions %q: RecommendedSQL = %q", opts, got[0].RecommendedSQL)
		}
		wantRB := `ALTER TABLE "public"."events" RESET (autovacuum_vacuum_scale_factor);`
		if got[0].RollbackSQL != wantRB {
			t.Errorf("reloptions %q: RollbackSQL = %q, want %q", opts,
				got[0].RollbackSQL, wantRB)
		}
		if _, ok := got[0].Detail["current_scale_factor"]; ok {
			t.Errorf("reloptions %q: current_scale_factor reported for an untuned table",
				opts)
		}
	}
}

// A DBA who turned autovacuum off for a table decided its maintenance;
// the rule does not tune what is not running.
func TestRuleAutovacuumTuning_AutovacuumDisabledSkipsTable(t *testing.T) {
	for _, v := range []string{"false", "off", "OFF", "of", "f", "no", "n", "0", "fal"} {
		opts := []string{"autovacuum_enabled=" + v, "autovacuum_vacuum_scale_factor=0.2"}
		snap := &collector.Snapshot{Tables: []collector.TableStats{avChurning(opts)}}
		if got := avFindings(t, snap); len(got) != 0 {
			t.Errorf("autovacuum_enabled=%s: SET recommended: %s", v,
				got[0].RecommendedSQL)
		}
	}
	for _, v := range []string{"true", "on", "yes", "1", "t", "TRUE"} {
		opts := []string{"autovacuum_enabled=" + v}
		snap := &collector.Snapshot{Tables: []collector.TableStats{avChurning(opts)}}
		if got := avFindings(t, snap); len(got) != 1 {
			t.Errorf("autovacuum_enabled=%s: %d findings, want 1", v, len(got))
		}
	}
}

// State transition: once pg_sage's own SET lands, the next cycle reads it
// back from the table and stops recommending it.
func TestRuleAutovacuumTuning_OwnSetStopsRefiring(t *testing.T) {
	snap := &collector.Snapshot{Tables: []collector.TableStats{avChurning(nil)}}
	first := avFindings(t, snap)
	if len(first) != 1 {
		t.Fatalf("first cycle findings = %d, want 1", len(first))
	}
	target, ok := first[0].Detail["recommended_scale_factor"].(float64)
	if !ok || target != 0.05 {
		t.Fatalf("recommended_scale_factor = %v, want 0.05",
			first[0].Detail["recommended_scale_factor"])
	}
	applied := "autovacuum_vacuum_scale_factor=" + strconv.FormatFloat(target, 'f', 2, 64)
	snap.Tables[0].Reloptions = []string{applied}
	if got := avFindings(t, snap); len(got) != 0 {
		t.Fatalf("after %s the rule re-fired: %s", applied, got[0].RecommendedSQL)
	}
}

// Each table is judged on its own reloptions.
func TestRuleAutovacuumTuning_PerTableReloptions(t *testing.T) {
	tuned := avChurning([]string{"autovacuum_vacuum_scale_factor=0.01"})
	loose := avTuneTable("public", "orders", 2_000_000, 3_000_000, 0)
	snap := &collector.Snapshot{Tables: []collector.TableStats{tuned, loose}}
	got := avFindings(t, snap)
	if len(got) != 1 || got[0].ObjectIdentifier != "public.orders" {
		t.Fatalf("findings = %+v, want only public.orders", got)
	}
}

// No concurrent-access test: ruleAutovacuumTuning is a pure function of
// its arguments and holds no shared state.

// parsePGBool matches PostgreSQL's parse_bool, checked against PG 17:
// ALTER TABLE ... SET (autovacuum_enabled = of) is stored as "of" and
// means false, while "o" and " on" are rejected.
func TestParsePGBool_MatchesPostgreSQL(t *testing.T) {
	cases := []struct {
		in        string
		val, okay bool
	}{
		{"of", false, true}, {"OFF", false, true}, {"on", true, true},
		{"t", true, true}, {"Tru", true, true}, {"y", true, true},
		{"f", false, true}, {"n", false, true}, {"1", true, true}, {"0", false, true},
		{"o", false, false}, {"offf", false, false}, {" on", false, false},
		{"10", false, false}, {"", false, false}, {"truee", false, false},
	}
	for _, c := range cases {
		val, okay := parsePGBool(c.in)
		if val != c.val || okay != c.okay {
			t.Errorf("parsePGBool(%q) = (%v, %v), want (%v, %v)",
				c.in, val, okay, c.val, c.okay)
		}
	}
	opts, ok := parseAutovacuumOptions([]string{"autovacuum_enabled=of"})
	if !ok || !opts.disabled {
		t.Errorf("autovacuum_enabled=of: opts %+v ok %v, want disabled", opts, ok)
	}
}

// PostgreSQL's parse_real (strtod) accepts whitespace around a real
// reloption and stores it verbatim: WITH (autovacuum_vacuum_scale_factor
// = ' 0.01') is kept as "autovacuum_vacuum_scale_factor= 0.01" (PG 17).
func TestRuleAutovacuumTuning_WhitespacePaddedScaleFactor(t *testing.T) {
	tuned := avChurning([]string{"autovacuum_vacuum_scale_factor= 0.01"})
	if got := avFindings(t, &collector.Snapshot{
		Tables: []collector.TableStats{tuned}}); len(got) != 0 {
		t.Fatalf("padded 0.01 re-recommended: %s", got[0].RecommendedSQL)
	}
	loose := avChurning([]string{"autovacuum_vacuum_scale_factor= 0.2 "})
	got := avFindings(t, &collector.Snapshot{Tables: []collector.TableStats{loose}})
	if len(got) != 1 {
		t.Fatalf("padded 0.2: %d findings, want 1", len(got))
	}
	// vacuum-07: the padded text is restored verbatim (quoted), so the
	// rollback leaves pg_class.reloptions exactly as it found it.
	want := `ALTER TABLE "public"."events" SET (autovacuum_vacuum_scale_factor = ' 0.2 ');`
	if got[0].RollbackSQL != want {
		t.Errorf("RollbackSQL = %q, want %q", got[0].RollbackSQL, want)
	}
}
