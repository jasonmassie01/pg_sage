package causal

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Wraparound runway / autovacuum starvation: what keeps a table's freeze
// horizon from advancing. A mechanism is supported only while the table
// is near its freeze maximum or the measured runway to the wraparound
// warning limit is short: long transactions and busy autovacuum are
// everyday sights, not incidents.

// Wraparound thresholds (product defaults, AI-SRE-SPEC §4 R2).
const (
	// NearFraction is the share of a table's freeze maximum at which its
	// runway counts as near.
	NearFraction = 0.5
	// WraparoundNearHorizon is the measured runway to the wraparound
	// warning limit that counts as near.
	WraparoundNearHorizon = 14 * 24 * time.Hour
	// pinShare: a holder whose xmin age is at least this share of the
	// table's XID age pins the table; under refuteShare it cannot.
	pinShare    = 0.5
	refuteShare = 0.1
	// An XID rate between samples at least xidSurgeRatio times its trend
	// (and at least xidSurgeFloor per second) is a surge; within
	// xidCalmRatio it is not.
	xidSurgeRatio = 3
	xidCalmRatio  = 1.5
	xidSurgeFloor = 1000
	// minTrendSamples is the fewest samples a trend is read from.
	minTrendSamples = 3
	mechWeight      = 0.3
	nearWeight      = 0.3
	oldestWeight    = 0.1
)

// holderKinds are the xmin-horizon holder kinds each node covers.
var holderKinds = map[NodeID][]string{
	XminHeldBySession:      {probes.HolderSession},
	XminHeldByPreparedXact: {probes.HolderPreparedXact},
	XminHeldByReplication: {probes.HolderSlot, probes.HolderSlotCatalog,
		probes.HolderStandby},
}

// wrapEvidence is what the wraparound family reads.
type wrapEvidence struct {
	xid       walEvidence
	table     *probes.WraparoundTable
	tableEv   string
	holders   []probes.XminHolder
	holdersEv string
	holdersOK bool
	cancels   int64
	cancelEv  string
	cancelOK  bool
	trend     probes.RunwayTrend
	trendEv   string
	trendOK   bool
	near      bool
	nearFact  Fact
	missing   []Missing
}

// DiagnoseWraparound scores the wraparound runway hypotheses for the
// subject ("table schema.name", "xid" or "mxid"; otherwise the table
// nearest its maximum).
func DiagnoseWraparound(obs []Observation, subject string) Diagnosis {
	w := readWraparound(obs, subject)
	if w.table == nil {
		d := Diagnosis{Family: FamilyWraparound, GraphVersion: GraphVersion,
			Subject: subject, Missing: w.missing,
			Reason: "table freeze horizons unavailable"}
		if name, ok := strings.CutPrefix(subject, "table "); ok && w.tableEv != "" {
			d.Reason = fmt.Sprintf("table %s is not among the tables nearest their "+
				"freeze maximum", name)
		}
		return d
	}
	hs := []Hypothesis{scoreHolder(w, XminHeldBySession),
		scoreHolder(w, XminHeldByPreparedXact), scoreHolder(w, XminHeldByReplication),
		scoreSaturated(w), scoreDisabled(w), scoreCancelled(w), scoreXIDSurge(w)}
	d := rank(FamilyWraparound, hs)
	d.Missing, d.Observed = w.missing, w.observed()
	if d.Subject == "" {
		d.Subject = "table " + w.table.Relation
	}
	if !w.near {
		d.Reason = "no table is near its freeze maximum and no measured runway to " +
			"the wraparound warning limit is short"
	}
	return d
}

func readWraparound(obs []Observation, subject string) wrapEvidence {
	var w wrapEvidence
	w.xid, w.missing = walSeries(obs, probes.XIDRunwayProbe)
	w.missing = append(w.missing, missingFor(obs, probes.WraparoundTablesProbe,
		probes.XminHorizon, probes.AutovacuumCancellations, probes.RunwayTrendsProbe)...)
	if o, ok := find(obs, probes.WraparoundTablesProbe); ok {
		if ts, err := probes.WraparoundTables(o.Result); err == nil {
			w.tableEv = o.EvidenceID
			w.table = pickTable(ts, subject)
		}
	}
	if o, ok := find(obs, probes.XminHorizon); ok {
		hs, err := probes.XminHolders(o.Result)
		w.holders, w.holdersEv, w.holdersOK = hs, o.EvidenceID, err == nil
	}
	if o, ok := find(obs, probes.AutovacuumCancellations); ok {
		n, err := probes.AutovacuumCancellationCount(o.Result)
		w.cancels, w.cancelEv, w.cancelOK = n, o.EvidenceID, err == nil
	}
	w.readTrend(obs, subject)
	if w.table != nil {
		w.nearness()
	}
	return w
}

func pickTable(ts []probes.WraparoundTable, subject string) *probes.WraparoundTable {
	name, byName := strings.CutPrefix(subject, "table ")
	for i := range ts {
		if !byName || ts[i].Relation == name {
			return &ts[i]
		}
	}
	return nil
}

// readTrend reads the cluster's XID (or, for an "mxid" subject,
// multixact) trend; a usable probe without that series is missing it.
func (w *wrapEvidence) readTrend(obs []Observation, subject string) {
	o, ok := find(obs, probes.RunwayTrendsProbe)
	if !ok || !o.Result.Status.Usable() {
		return
	}
	kind := probes.RunwayXID
	if subject == "mxid" {
		kind = probes.RunwayMXID
	}
	ts, err := probes.RunwayTrends(o.Result)
	tr, found := probes.FindTrend(ts, kind, probes.SubjectCluster)
	if err != nil || !found || tr.Samples < minTrendSamples {
		w.missing = append(w.missing, Missing{ProbeID: probes.RunwayTrendsProbe,
			Reason: kind + "_trend_unavailable"})
		return
	}
	w.trend, w.trendEv, w.trendOK = tr, o.EvidenceID, true
}

// nearness decides whether the runway is near: the table at half its
// maximum, or the measured runway to the warning limit inside the
// horizon.
func (w *wrapEvidence) nearness() {
	if w.table.Fraction() >= NearFraction {
		w.near, w.nearFact = true, Fact{EvidenceID: w.tableEv, Text: tableText(*w.table)}
		return
	}
	if w.trendOK && w.trend.SecondsToLimit() <= WraparoundNearHorizon.Seconds() {
		w.near, w.nearFact = true, Fact{EvidenceID: w.trendEv, Text: wrapTrendText(w.trend)}
	}
}

func (w wrapEvidence) observed() []Fact {
	out := []Fact{{EvidenceID: w.tableEv, Text: tableText(*w.table)}}
	if s := w.trend.SecondsToLimit(); w.trendOK && !math.IsNaN(s) && !math.IsInf(s, 0) {
		out = append(out, Fact{EvidenceID: w.trendEv, Text: wrapTrendText(w.trend)})
	}
	return out
}

func tableText(t probes.WraparoundTable) string {
	age, max, what := t.XIDAge, t.FreezeMaxAge, "XID"
	if probes.Known(t.MXIDAge) && t.MXIDFreezeMaxAge > 0 &&
		t.MXIDAge/t.MXIDFreezeMaxAge > t.XIDAge/t.FreezeMaxAge {
		age, max, what = t.MXIDAge, t.MXIDFreezeMaxAge, "multixact"
	}
	return fmt.Sprintf("table %s has %s age %s, %s times its freeze maximum of %s",
		t.Relation, what, probes.FormatValue(age), probes.FormatValue(t.Fraction()),
		probes.FormatValue(max))
}

func wrapTrendText(tr probes.RunwayTrend) string {
	what := "XID"
	if tr.Kind == probes.RunwayMXID {
		what = "multixact"
	}
	return fmt.Sprintf("the cluster's %s age %s reaches the wraparound warning limit %s "+
		"in %s s at %s per second (%d samples over %s s)", what,
		probes.FormatValue(tr.LastValue), probes.FormatValue(tr.Limit),
		probes.FormatValue(tr.SecondsToLimit()), probes.FormatValue(tr.RatePerS),
		tr.Samples, probes.FormatValue(tr.SpanS()))
}

// addNear adds the nearness fact to a supported mechanism.
func (w wrapEvidence) addNear(h *Hypothesis) {
	if w.near {
		h.add(nearWeight, w.nearFact.EvidenceID, w.nearFact.Text)
	}
}
