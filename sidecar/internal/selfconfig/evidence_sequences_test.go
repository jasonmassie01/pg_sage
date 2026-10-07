package selfconfig

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// The sequence evidence times a bounded sample of last-value reads and
// scales it by the sequence count (perf gate: reading all 5,000 sequences
// cold took 986 ms at startup, over the 500 ms catalog budget).

func TestSequenceSampleSQLIsBounded(t *testing.T) {
	if !strings.Contains(sequenceSampleSQL, "LIMIT $1") {
		t.Fatalf("sequence sample is not bounded:\n%s", sequenceSampleSQL)
	}
	if !strings.Contains(sequenceSampleSQL, "count(last_value)") {
		t.Fatalf("sample must read each sampled last value:\n%s", sequenceSampleSQL)
	}
	if strings.Contains(sequenceCountSQL, "last_value") {
		t.Fatalf("the count must not read last values:\n%s", sequenceCountSQL)
	}
	if sequenceSampleSize < 50 || sequenceSampleSize > 1000 {
		t.Fatalf("sample size %d, want a few hundred", sequenceSampleSize)
	}
}

func TestExtrapolateSequenceScan(t *testing.T) {
	cases := []struct {
		ms, sampled, total, want float64
	}{
		{10, 250, 5000, 200},  // linear in the sequence count
		{10, 250, 250, 10},    // everything sampled: the measurement itself
		{10, 100, 100, 10},    // fewer sequences than the sample size
		{0, 250, 5000, 0},     // instant reads
		{7, 0, 0, 0},          // no sequence: nothing to read
		{7, 0, 5000, 7},       // none readable in the sample: keep what was measured
		{-1, 250, 5000, 0},    // a negative clock reading is not evidence of speed
		{10, 250, 249, 10},    // a count below the sample (concurrent DROP) never shrinks it
		{1.5, 3, 30000, 15000}, // the property the rule sizes against
	}
	for _, c := range cases {
		got := extrapolateSequenceScan(c.ms, c.sampled, c.total)
		if got != c.want {
			t.Errorf("extrapolate(%v ms, %v of %v) = %v, want %v",
				c.ms, c.sampled, c.total, got, c.want)
		}
	}
}

// More sequences than the sample: the count is exact, the scan time is the
// sample's scaled up.
func TestGatherSamplesSequences(t *testing.T) {
	pool, ctx := testPool(t)
	stmts := []string{"DROP SCHEMA IF EXISTS sc_seqsample CASCADE", "CREATE SCHEMA sc_seqsample"}
	for i := 0; i < sequenceSampleSize+40; i++ {
		stmts = append(stmts, fmt.Sprintf("CREATE SEQUENCE sc_seqsample.s%d", i))
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS sc_seqsample CASCADE")
	})
	var want float64
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM pg_sequences").Scan(&want); err != nil {
		t.Fatal(err)
	}
	ev, err := Gather(ctx, pool)
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if !ev.Sequences.Known || ev.Sequences.Value != want {
		t.Fatalf("sequences %+v, want %v", ev.Sequences, want)
	}
	if !ev.SequenceScanMs.Known || ev.SequenceScanMs.Value < 0 {
		t.Fatalf("sequence scan %+v", ev.SequenceScanMs)
	}
	var sampled float64
	if err := pool.QueryRow(ctx, sequenceSampleSQL, sequenceSampleSize).
		Scan(&sampled, new(float64)); err != nil {
		t.Fatal(err)
	}
	if sampled != float64(sequenceSampleSize) {
		t.Fatalf("sample read %v sequences, want %d", sampled, sequenceSampleSize)
	}
}
