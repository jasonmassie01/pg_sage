package causal

import (
	"fmt"
	"math"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// runwayFacts are the disk and slot runways the sampled trends project:
// disk usage against the declared capacity, and each growing slot
// against its configured WAL maximum. Projections that never reach their
// limit, or whose limit is unknown, are not stated.
func runwayFacts(ts []probes.RunwayTrend, ev string) []Fact {
	var out []Fact
	for _, tr := range ts {
		s := tr.SecondsToLimit()
		if math.IsNaN(s) || math.IsInf(s, 0) || tr.Samples < minTrendSamples {
			continue
		}
		switch tr.Kind {
		case probes.RunwayDiskUsed:
			out = append(out, Fact{EvidenceID: ev, Text: fmt.Sprintf("disk usage %s bytes "+
				"of the declared capacity %s bytes; at %s it is full in %s s",
				probes.FormatValue(tr.LastValue), probes.FormatValue(tr.Limit),
				trendText(tr), probes.FormatValue(s))})
		case probes.RunwayWALSlot:
			out = append(out, Fact{EvidenceID: ev, Text: fmt.Sprintf("slot %q retains %s "+
				"bytes of its configured maximum %s bytes; at %s it reaches it in %s s",
				tr.Subject, probes.FormatValue(tr.LastValue), probes.FormatValue(tr.Limit),
				trendText(tr), probes.FormatValue(s))})
		}
	}
	return out
}
