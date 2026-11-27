package publish

import (
	"crypto/ed25519"
	"crypto/sha256"
	"time"

	"github.com/pg-sage/sidecar/internal/outcomes"
	"github.com/pg-sage/sidecar/internal/verify/backtest"
)

// The sample bundle (docs/outcomes-sample) shows the format: a fixture
// ledger of 40 changes, a fixed clock and a sample key whose private half
// anyone can derive, so it proves nothing about any install.

// SampleNow is the sample bundle's clock.
var SampleNow = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

// sampleSpec is n changes of one class, verdict, rollback state and origin.
type sampleSpec struct {
	actionType, verdict, rollback, origin string
	n                                     int
}

const (
	sCI = "create_index_concurrently"
	sAV = outcomes.RollbackAvailable
	sNR = outcomes.RollbackNotReversible
	sRB = outcomes.RollbackRolledBack
)

var sampleSpecs = []sampleSpec{
	{sCI, outcomes.VerdictImproved, sAV, outcomes.OriginAutonomous, 5},
	{sCI, outcomes.VerdictImproved, sAV, outcomes.OriginApproved, 2},
	{sCI, outcomes.VerdictImproved, sRB, outcomes.OriginApproved, 1},
	{sCI, outcomes.VerdictNoChange, sAV, outcomes.OriginAutonomous, 2},
	{sCI, outcomes.VerdictRegressed, sRB, outcomes.OriginAutonomous, 1},
	{sCI, outcomes.VerdictInconclusive, sAV, outcomes.OriginPlan, 1},
	{"analyze_table", outcomes.VerdictImproved, sNR, outcomes.OriginAutonomous, 6},
	{"analyze_table", outcomes.VerdictNoChange, sNR, outcomes.OriginAutonomous, 3},
	{"analyze_table", outcomes.VerdictPending, sNR, outcomes.OriginAutonomous, 1},
	{"vacuum_table", outcomes.VerdictImproved, sNR, outcomes.OriginAutonomous, 3},
	{"vacuum_table", outcomes.VerdictNoChange, sNR, outcomes.OriginApproved, 2},
	{"vacuum_table", outcomes.VerdictUnverifiable, sNR, outcomes.OriginAutonomous, 1},
	{"vacuum_table", outcomes.VerdictExpired, sNR, outcomes.OriginAutonomous, 1},
	{"drop_unused_index", outcomes.VerdictNoChange, sAV, outcomes.OriginApproved, 4},
	{"drop_unused_index", outcomes.VerdictRegressed, sRB, outcomes.OriginApproved, 1},
	{"set_table_autovacuum", outcomes.VerdictImproved, sAV, outcomes.OriginPlan, 2},
	{"set_table_autovacuum", outcomes.VerdictNoChange, sAV, outcomes.OriginPlan, 1},
	{"alter_system_guc", outcomes.VerdictImproved, sAV, outcomes.OriginApproved, 1},
	{"alter_system_guc", outcomes.VerdictRegressed, outcomes.RollbackFailed,
		outcomes.OriginAgent, 1},
	{"experimental_step", outcomes.VerdictImproved, sNR, outcomes.OriginAutonomous, 1},
}

// SampleRows is the sample's fixture ledger: 40 changes, one an hour back
// from SampleNow.
func SampleRows() []outcomes.Row {
	var rows []outcomes.Row
	for _, s := range sampleSpecs {
		for range s.n {
			rows = append(rows, outcomes.Row{ActionID: int64(len(rows) + 1),
				Database: "sample", ActionType: s.actionType, Verdict: s.verdict,
				RollbackState: s.rollback, Origin: s.origin,
				ExecutedAt: SampleNow.Add(-time.Duration(len(rows)+1) * time.Hour)})
		}
	}
	return rows
}

// SampleBundle is the sample bundle: the fixture ledger over 90 days and
// one back-test of 300 placebos with 9 false positives.
func SampleBundle() Bundle {
	return Build(Input{Rows: SampleRows(), Databases: 1, Since: 90 * 24 * time.Hour,
		Now: SampleNow, Sample: true,
		Backtests: []backtest.Counts{{Placebos: 300, Excluded: 12, FalsePositives: 9}}})
}

// SampleKey is the sample's signing key, derived from a published seed.
// Never trust a bundle signed with it.
func SampleKey() ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("pg_sage outcomes sample key: never trust it"))
	return ed25519.NewKeyFromSeed(seed[:])
}
