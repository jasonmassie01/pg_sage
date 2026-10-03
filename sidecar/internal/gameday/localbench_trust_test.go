package gameday

import (
	"context"
	"errors"
	"testing"
)

// Roadmap 1.2: the self-initiated trust families (tuning, hygiene) are
// ledger families, but no incident bench covers them; a local bench run
// for one is refused like an unknown family.
func TestLocalBenchRefusesSelfInitiatedFamilies(t *testing.T) {
	p := &fakeProvider{}
	b := newLocalBench(t, p, &fakeFaults{}, &fakeBenchLedger{})
	for _, families := range [][]string{{"tuning"}, {"wal_retention", "hygiene"}} {
		if _, err := b.Run(context.Background(), families); !errors.Is(err,
			ErrUnknownFamily) {
			t.Errorf("%v: err = %v, want ErrUnknownFamily", families, err)
		}
	}
	if len(p.created) != 0 {
		t.Fatal("a clone was created for a self-initiated family")
	}
}
