package causal

import (
	"testing"
	"time"
)

// Contradictory samples (Sage SRE M4, replay corpus): two different
// connection_saturation samples taken at the same instant cannot both be
// true, so growth between them is not evidence. (Samples stored out of
// order are sorted by observation time first, so they still compare.)
// The comparison is invalid and stated as missing evidence, like a
// restart between the samples (CHECK-07).

func TestConnections_SamplesAtTheSameInstantAreNotCompared(t *testing.T) {
	for name, second := range map[string]time.Time{"same instant": t0} {
		t.Run(name, func(t *testing.T) {
			d := DiagnoseConnections([]Observation{
				connObs("E1", t0, 12, group{app: "etl", state: "idle", n: 2}),
				connObs("E2", second, 12, group{app: "etl", state: "idle", n: 9}),
			})
			if d.Conclusive || d.Root != nil {
				t.Fatalf("contradictory samples concluded %+v", d.Root)
			}
			leak, st := byNode(d, ConnectionLeak)
			if st == StatusRuledOut || leak.Confidence != 0 {
				t.Fatalf("leak scored from contradictory samples: %s %+v", st, leak)
			}
			found := false
			for _, m := range d.Missing {
				if m.Reason == "samples_out_of_order" {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing %+v lacks samples_out_of_order", d.Missing)
			}
		})
	}
}

// The ordinary case still compares: 5 s apart, growth of 7 is a leak.
func TestConnections_OrderedSamplesStillCompare(t *testing.T) {
	d := DiagnoseConnections([]Observation{
		connObs("E1", t0, 12, group{app: "etl", state: "idle", n: 2}),
		connObs("E2", t0.Add(5*time.Second), 12, group{app: "etl", state: "idle", n: 9}),
	})
	requireStatus(t, d, ConnectionLeak, StatusRoot)
}
