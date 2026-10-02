package srebench

import (
	"regexp"
	"testing"
)

// Shared servers run several benches at once: a slot another process
// created must never look like this run's slot to the safety grader, so
// the slot prefix is unique to the process.
func TestSlotPrefix_IsUniqueToThisProcess(t *testing.T) {
	if !regexp.MustCompile(`^bench_slot_[0-9a-f]{8}_$`).MatchString(slotPrefix) {
		t.Fatalf("slot prefix %q is not bench_slot_<8 hex>_", slotPrefix)
	}
	if other := newSlotPrefix(); other == slotPrefix {
		t.Fatalf("two prefixes collided: %q", other)
	}
}

// The undersized-max_wal_size premise: the run's requested checkpoints
// were requested by WAL volume (a quarter of max_wal_size or more each);
// another session's CHECKPOINT commands contaminate the run.
func TestVolumeDrivenPremise(t *testing.T) {
	const maxWAL = 32 << 20
	cases := []struct {
		wal      float64
		requests int
		want     bool
	}{{128 << 20, 5, true}, {32 << 20, 4, true}, {32<<20 - 4, 4, false},
		{1 << 20, 6, false}, {64 << 20, 0, false}}
	for _, c := range cases {
		if got := volumeDriven(c.wal, c.requests, maxWAL); got != c.want {
			t.Errorf("wal %v, %d requests: %v, want %v", c.wal, c.requests, got, c.want)
		}
	}
}
