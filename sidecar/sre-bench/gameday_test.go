package srebench

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Game days (AI-SRE-SPEC §4 R3) run PGIncidentBench fault programs
// against a disposable clone of a customer database with the
// deterministic investigator (no model tokens are spent on clones) and
// hand back the same JSON report the ledger ingests.

func TestSelectGameDayScenarios(t *testing.T) {
	all, err := selectGameDayScenarios(GameDayOptions{})
	if err != nil || len(all) != len(Scenarios()) {
		t.Fatalf("no filter = %d scenarios (%v), want all %d", len(all), err, len(Scenarios()))
	}
	lock, err := selectGameDayScenarios(GameDayOptions{Families: []string{"lock_blocking"}})
	if err != nil || len(lock) == 0 {
		t.Fatalf("lock_blocking = %d (%v)", len(lock), err)
	}
	for _, sc := range lock {
		if string(sc.Family) != "lock_blocking" {
			t.Fatalf("family filter leaked %s", sc.ID)
		}
	}
	one, err := selectGameDayScenarios(GameDayOptions{Scenarios: []string{"lock-hot-row"}})
	if err != nil || len(one) != 1 || one[0].ID != "lock-hot-row" {
		t.Fatalf("scenario filter = %+v (%v)", one, err)
	}
	for name, opts := range map[string]GameDayOptions{
		"unknown family":   {Families: []string{"shell"}},
		"unknown scenario": {Scenarios: []string{"rm-rf"}},
		"empty selection": {Families: []string{"wal_retention"},
			Scenarios: []string{"lock-hot-row"}},
	} {
		if _, err := selectGameDayScenarios(opts); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestRunGameDayValidatesBeforeConnecting(t *testing.T) {
	_, err := RunGameDay(context.Background(), "postgres://nobody@127.0.0.1:1/x",
		GameDayOptions{Families: []string{"shell"}}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "shell") {
		t.Fatalf("unknown family: %v", err)
	}
	if _, err := RunGameDay(context.Background(), "", GameDayOptions{}, time.Now()); err == nil {
		t.Fatal("an empty DSN was accepted")
	}
}

func TestRunGameDayProducesAnIngestibleReport(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	now := time.Now().UTC()
	raw, err := RunGameDay(ctx, dsn, GameDayOptions{Families: []string{"lock_blocking"},
		Scenarios: []string{"lock-idle-row-holder"}}, now)
	if err != nil {
		t.Fatalf("game day: %v", err)
	}
	run, err := earned.ParseBenchReport(raw, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("the ledger cannot read the game-day report: %v", err)
	}
	if strings.Join(run.Gated, ",") != ArmCausalGraph {
		t.Fatalf("gated arms = %v, want only the deterministic investigator", run.Gated)
	}
	var cell *earned.Cell
	for i := range run.Cells {
		c := run.Cells[i]
		if c.Arm == ArmLLM || strings.HasPrefix(c.Arm, "rules") {
			t.Fatalf("a game day ran arm %s", c.Arm)
		}
		if c.Family == "lock_blocking" {
			cell = &run.Cells[i]
		}
	}
	if cell == nil || cell.Runs != 1 || cell.SafePass.N != 1 || cell.Forbidden != 0 {
		t.Fatalf("lock_blocking cell = %+v", cell)
	}
}

func TestGameDayFaultsAdapter(t *testing.T) {
	f := GameDayFaults{Scenarios: []string{"lock-hot-row"}}
	if _, err := f.Run(context.Background(), "", []string{"lock_blocking"}); err == nil {
		t.Fatal("the adapter accepted an empty DSN")
	}
}
