package causal

import (
	"fmt"
	"math"
	"sort"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Graph v3 (Sage SRE M5): the change feed and the SLO status.

// RecentChange is a change recorded by the change feed in the window.
const RecentChange NodeID = "recent_change"

// FamilySLO is the family of an SLO-burn triage diagnosis.
const FamilySLO Family = "slo_burn"

var v3Nodes = []Node{
	{ID: RecentChange, Family: FamilyChange, Label: "recent change",
		Mechanism: "A deploy, migration, feature flag, configuration, DDL, statistics " +
			"reset, restart, failover or extension change shortly before the incident " +
			"caused or worsened it.",
		Predicted:   "a change in the change feed in the window before the incident",
		Refutation:  string(probes.ChangeFeed),
		Confounders: "any change is correlation until the mechanism is shown",
		OperatorStep: "Review the changes in the window with their owners; roll back " +
			"the suspect one through your normal process if the evidence points to it."},
}

// Change scoring: a change is a question to check, never a root alone.
const (
	changeWeight   = 0.1
	maxChangeFacts = 3
)

// WithChanges adds the recent-change hypothesis to a diagnosis when the
// change feed was collected: supported by the changes in the window
// (other than pg_sage's own actions, which are their own hypothesis),
// ruled out by an observed absence of changes, unproven (and missing)
// when the feed could not be read.
func WithChanges(d Diagnosis, obs []Observation) Diagnosis {
	o, ok := find(obs, probes.ChangeFeed)
	if !ok {
		return d
	}
	h := newHypothesis(RecentChange, "change feed")
	h.Status = StatusAlternative
	rows, err := probes.ChangeRows(o.Result)
	if err != nil {
		d.Missing = append(d.Missing, Missing{ProbeID: probes.ChangeFeed,
			Status: o.Result.Status, Reason: unavailableReason(o, err)})
		d.Alternatives = append(d.Alternatives, h)
		return d
	}
	var changes []probes.ChangeRow
	for _, r := range rows {
		if r.Kind != "sage_action" {
			changes = append(changes, r)
		}
	}
	sort.SliceStable(changes, func(i, j int) bool { return changes[i].AgeS < changes[j].AgeS })
	if len(changes) == 0 {
		h.contradict(o.EvidenceID, "the change feed recorded no change other than "+
			"pg_sage's own actions in the window")
		h.Status = StatusRuledOut
		d.RuledOut = append(d.RuledOut, h)
		return d
	}
	for i, c := range changes {
		if i == maxChangeFacts {
			break
		}
		h.add(changeWeight, o.EvidenceID, fmt.Sprintf("%s change from %s (%s): %s, %s s "+
			"before the probe", probes.FormatValue(c.Kind), probes.FormatValue(c.Source),
			probes.FormatValue(c.Signature), probes.FormatValue(c.Summary),
			probes.FormatValue(c.AgeS)))
	}
	d.Alternatives = append(d.Alternatives, h)
	return d
}

// Customer-impact states.
const (
	ImpactBurning    = "burning"
	ImpactNotBurning = "not_burning"
	ImpactUnknown    = "unknown"
	ImpactNoAppSLO   = "no_app_slo"
)

// Impact is the customer-impact claim bound to slo_status evidence: only
// a registered app SLI can claim it; proxies never do.
type Impact struct {
	State      string
	SLO        string
	EvidenceID string
	BurnRate   *float64
	Window     string
	Reason     string
}

// maxSLOFacts bounds the SLO facts a diagnosis observes.
const maxSLOFacts = 4

// WithSLO binds the SLO status (when it was collected) to a diagnosis:
// each SLO's state becomes an observed fact (proxies labeled as such)
// and the app SLIs decide the customer-impact claim.
func WithSLO(d Diagnosis, obs []Observation) Diagnosis {
	o, ok := find(obs, probes.SLOStatus)
	if !ok {
		return d
	}
	rows, err := probes.SLORows(o.Result)
	if err != nil {
		d.Missing = append(d.Missing, Missing{ProbeID: probes.SLOStatus,
			Status: o.Result.Status, Reason: unavailableReason(o, err)})
		d.Impact = &Impact{State: ImpactUnknown, EvidenceID: o.EvidenceID,
			Reason: unavailableReason(o, err)}
		return d
	}
	for i, r := range rows {
		if i == maxSLOFacts {
			break
		}
		d.Observed = append(d.Observed, Fact{EvidenceID: o.EvidenceID, Text: sloFact(r)})
	}
	d.Impact = impactOf(rows, o.EvidenceID)
	return d
}

func sloFact(r probes.SLORow) string {
	who := "app SLO " + probes.FormatValue(r.Name)
	if r.Kind != "app" {
		who = "database proxy SLO " + probes.FormatValue(r.Name) + " (a proxy, not " +
			"customer impact)"
	}
	if r.State == "unknown" || math.IsNaN(r.BurnLong) || math.IsNaN(r.BurnShort) {
		return fmt.Sprintf("%s is %s (%s)", who, probes.FormatValue(r.State),
			probes.FormatValue(r.Unknown))
	}
	return fmt.Sprintf("%s is %s: burn rate %sx over %s and %sx over %s", who,
		probes.FormatValue(r.State), probes.FormatValue(r.BurnLong),
		probes.FormatValue(r.LongWindow), probes.FormatValue(r.BurnShort),
		probes.FormatValue(r.ShortWindow))
}

// impactOf: a burning app SLI claims impact (the fastest burn); an
// unknown one leaves it unknown; otherwise no app SLI burns. Without
// any app SLI there is nothing to claim.
func impactOf(rows []probes.SLORow, evidenceID string) *Impact {
	var burning, unknown *probes.SLORow
	apps := 0
	for i := range rows {
		r := &rows[i]
		if r.Kind != "app" {
			continue
		}
		apps++
		switch {
		case r.State == "page" || r.State == "ticket":
			if burning == nil || r.BurnLong > burning.BurnLong {
				burning = r
			}
		case r.State == "unknown" && unknown == nil:
			unknown = r
		}
	}
	switch {
	case apps == 0:
		return &Impact{State: ImpactNoAppSLO, EvidenceID: evidenceID}
	case burning != nil:
		im := &Impact{State: ImpactBurning, SLO: burning.Name, EvidenceID: evidenceID,
			Window: burning.LongWindow}
		if !math.IsNaN(burning.BurnLong) {
			b := burning.BurnLong
			im.BurnRate = &b
		}
		return im
	case unknown != nil:
		return &Impact{State: ImpactUnknown, SLO: unknown.Name, EvidenceID: evidenceID,
			Reason: unknown.Unknown}
	}
	return &Impact{State: ImpactNotBurning, EvidenceID: evidenceID}
}
