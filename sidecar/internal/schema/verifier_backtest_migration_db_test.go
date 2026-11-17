package schema

import (
	"context"
	"strings"
	"testing"
)

// lab-02 on a real server: bootstrap twice is clean and writes no row, and
// the checks refuse counts that cannot be (a negative count, excluded or
// false positives beyond the placebos, a rate outside [0, 1]). No
// concurrent-writer test: Bootstrap holds its advisory lock.
func TestVerifierBacktestMigration_BootstrapTwiceAndConstraints(t *testing.T) {
	pool := freshSchemaPool(t, "verifierbacktest")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.verifier_backtest`).
		Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows after bootstrap = %d, %v; want 0", rows, err)
	}
	insert := func(placebos, excluded, fps int, rate float64) error {
		_, err := pool.Exec(ctx, `INSERT INTO sage.verifier_backtest (ran_at,
			window_from, window_to, verifier, placebos, excluded, false_positives,
			fp_rate, fp_wilson_upper, inconclusive_share, injected, detected, seed, report)
			VALUES (now(), now() - interval '90 days', now(), 'v2', $1, $2, $3, $4, 0.02,
			0.1, 300, 290, 1, '{}')`, placebos, excluded, fps, rate)
		return err
	}
	if err := insert(300, 2, 3, 0.01); err != nil {
		t.Fatalf("a valid row was refused: %v", err)
	}
	for name, err := range map[string]error{
		"negative placebos":      insert(-1, 0, 0, 0),
		"excluded beyond":        insert(10, 11, 0, 0),
		"false positives beyond": insert(10, 0, 11, 0),
		"rate above 1":           insert(300, 0, 3, 1.5),
	} {
		if err == nil || !strings.Contains(err.Error(), "check") {
			t.Errorf("%s: %v, want a check violation", name, err)
		}
	}
}
