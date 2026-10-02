package causal

import (
	"fmt"
	"math"
	"sort"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// usableSeries returns the usable observations of one probe in time
// order, and the missing evidence: each distinct failure once, or
// not_collected when the probe never ran.
func usableSeries(obs []Observation, id probes.ID) ([]Observation, []Missing) {
	var out []Observation
	var missing []Missing
	seen := map[string]bool{}
	for _, o := range obs {
		if o.Result.ProbeID != id {
			continue
		}
		if o.Result.Status.Usable() {
			out = append(out, o)
			continue
		}
		key := string(o.Result.Status) + "/" + o.Result.Reason
		if !seen[key] {
			seen[key] = true
			missing = append(missing, Missing{ProbeID: id, Status: o.Result.Status,
				Reason: o.Result.Reason})
		}
	}
	if len(out) == 0 && len(missing) == 0 {
		missing = []Missing{{ProbeID: id, Reason: "not_collected"}}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Result.ObservedAt.Before(out[j].Result.ObservedAt)
	})
	return out, missing
}

// spanSeconds is the time between two observations in seconds.
func spanSeconds(a, b Observation) float64 {
	return b.Result.ObservedAt.Sub(a.Result.ObservedAt).Seconds()
}

// fv renders a number the way evidence renders it.
func fv(v float64) string { return probes.FormatValue(v) }

// pct renders a share as a whole percentage.
func pct(share float64) string { return fv(math.Round(share * 100)) }

// burstScore scores a WAL rate against the burst floor and its long-run
// average (unknown when NaN): support, a contradiction or neither.
func burstScore(h *Hypothesis, ev string, rate, span, avg float64) {
	avgKnown := probes.Known(avg) && avg > 0
	text := fmt.Sprintf("WAL was written at %s bytes/s over %s s", fv(math.Round(rate)),
		fv(span))
	switch {
	case rate >= surgeMinRate && (!avgKnown || rate >= surgeRatio*avg):
		if avgKnown {
			text += fmt.Sprintf(", %s times its average of %s bytes/s", fv(math.Round(rate/avg)),
				fv(math.Round(avg)))
		}
		h.add(0.5, ev, text)
		if avgKnown && rate >= strongSurgeRatio*avg {
			h.add(0.2, ev, "that is at least 10 times its long-run rate")
		}
	case avgKnown && rate < calmRatio*avg:
		h.contradict(ev, text+fmt.Sprintf(", within 1.5 times its average of %s bytes/s",
			fv(math.Round(avg))))
	case !avgKnown && rate < surgeMinRate:
		h.contradict(ev, text+", under the burst floor of 4194304 bytes/s")
	}
}
