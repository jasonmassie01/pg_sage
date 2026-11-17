package upgradecheck

import (
	"time"

	"github.com/pg-sage/sidecar/internal/pgversion"
)

// The upgrade brief (drill-02a) turns one readiness run into a dated plan
// per database: what blocks the upgrade, each extension's path, each
// logical slot's plan, the indexes to reindex, what waiting costs at the
// operator's rates, and an ordered checklist. It is read-only like the
// checks: the reindex steps are future executor steps and the upgrade
// itself stays with the operator.

// Support states of a major on its provider.
const (
	SupportSupported = "supported"
	SupportEnded     = "ended"
	SupportUnknown   = "unknown"
)

// Section states: known, or unknown with the reason (and SQLSTATE).
const (
	SectionKnown   = "known"
	SectionUnknown = "unknown"
)

// NoBlockersFound is the blocker summary of a brief with none.
const NoBlockersFound = "no blockers found"

// Cost-of-waiting states.
const (
	CostPriced        = "priced"
	CostUnpriced      = "unpriced"
	CostNotApplicable = "not_applicable"
)

// ReindexAction is the executor action a reindex step becomes
// (internal/executor/contract_maintenance.go).
const ReindexAction = "reindex_concurrently"

// SupportState is one major's support on its provider at the build time.
type SupportState struct {
	Provider string `json:"provider"`
	Major    int    `json:"major"`
	Ends     string `json:"ends,omitempty"`
	Status   string `json:"status"`
	Source   string `json:"source,omitempty"`
}

// BriefVersion is the source or target side of the upgrade.
type BriefVersion struct {
	Major      int          `json:"major"`
	VersionNum int          `json:"version_num,omitempty"`
	Support    SupportState `json:"support"`
}

// Section is a brief section's state.
type Section struct {
	State    string `json:"state"`
	Reason   string `json:"reason"`
	SQLState string `json:"sqlstate,omitempty"`
}

// Blocker is one thing that stops the upgrade.
type Blocker struct {
	Check  string `json:"check"`
	Title  string `json:"title"`
	Reason string `json:"reason"`
	Total  int    `json:"total"`
}

// BlockerSection lists the blockers; Unknown names checks that could not
// tell, so "no blockers found" never hides them.
type BlockerSection struct {
	Section
	Summary string    `json:"summary"`
	Items   []Blocker `json:"items"`
	Unknown []string  `json:"unknown_checks,omitempty"`
}

// ExtensionStep is one installed extension's path to the target.
type ExtensionStep struct {
	Extension        string   `json:"extension"`
	InstalledVersion string   `json:"installed_version"`
	TargetVersions   []string `json:"target_versions"`
	Path             string   `json:"path"`
	Blocker          bool     `json:"blocker"`
}

// ExtensionSection is every installed extension's path.
type ExtensionSection struct {
	Section
	Items []ExtensionStep `json:"items"`
}

// SlotStep is one logical slot's or subscription's plan.
type SlotStep struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Plugin   string `json:"plugin,omitempty"`
	Consumer string `json:"consumer"`
	Step     string `json:"step"`
}

// SlotSection is the slot and CDC plan.
type SlotSection struct {
	Section
	Items []SlotStep `json:"items"`
	Total int        `json:"total"`
}

// ReindexStep is one index to rebuild, a future executor step.
type ReindexStep struct {
	Action     string   `json:"action"`
	Index      string   `json:"index"`
	Table      string   `json:"table,omitempty"`
	Collations []string `json:"collations,omitempty"`
	Reason     string   `json:"reason"`
}

// ReindexSection is the collation reindex list.
type ReindexSection struct {
	Section
	Items   []ReindexStep `json:"items"`
	Total   int           `json:"total"`
	Skipped []string      `json:"skipped,omitempty"`
}

// HoursEstimate is the DBA work the plan needs, an estimate with its
// formula (decision D4: hours lead, dollars where rates exist).
type HoursEstimate struct {
	Low     float64 `json:"low"`
	High    float64 `json:"high"`
	Formula string  `json:"formula"`
}

// CostOfWaiting is what staying on the source major costs: extended
// support months at the operator's rate, never a shipped price.
type CostOfWaiting struct {
	Status              string        `json:"status"`
	Reason              string        `json:"reason"`
	ExtendedSupportFrom string        `json:"extended_support_from,omitempty"`
	Months              int           `json:"months"`
	PerMonth            float64       `json:"per_month"`
	Accrued             float64       `json:"accrued"`
	Currency            string        `json:"currency,omitempty"`
	RateAsOf            string        `json:"rate_as_of,omitempty"`
	Formula             string        `json:"formula,omitempty"`
	DBAHours            HoursEstimate `json:"dba_hours"`
}

// ChecklistStep is one ordered step; Owner is "operator" or "pg_sage".
type ChecklistStep struct {
	Order  int    `json:"order"`
	Step   string `json:"step"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Owner  string `json:"owner"`
}

// Brief is one database's dated upgrade plan.
type Brief struct {
	Database         string           `json:"database"`
	Source           BriefVersion     `json:"source"`
	Target           BriefVersion     `json:"target"`
	Method           string           `json:"method"`
	Provider         string           `json:"provider"`
	TargetDatabase   string           `json:"target_database,omitempty"`
	BuiltAt          time.Time        `json:"built_at"`
	Blockers         BlockerSection   `json:"blockers"`
	ExtensionPath    ExtensionSection `json:"extension_path"`
	SlotPlan         SlotSection      `json:"slot_plan"`
	CollationReindex ReindexSection   `json:"collation_reindex"`
	CostOfWaiting    CostOfWaiting    `json:"cost_of_waiting"`
	Checklist        []ChecklistStep  `json:"checklist"`
	// Hash identifies the content (BuiltAt excluded); the store keeps a
	// brief only when it differs from the latest.
	Hash string `json:"hash"`
}

// ExtendedSupportRate is the operator's extended-support price
// (cost.rates[].extended_support_per_vcpu_hour) and the instance's vCPUs.
type ExtendedSupportRate struct {
	PerVCPUHour float64
	VCPUs       int
	Currency    string
	AsOf        string
}

// BriefOptions tune one brief.
type BriefOptions struct {
	Options
	// NewestOfferedMajor is the newest major the provider offers, when a
	// cloud adapter reports it; 0 is the newest community major.
	NewestOfferedMajor int
	// SlotConsumers are the confirmed slot_consumer facts, slot subject
	// ('*' matches any characters) to consumer.
	SlotConsumers map[string]string
	// SlotConsumersErr is why the facts could not be read; the consumers
	// are then unknown.
	SlotConsumersErr error
	// Rate prices the cost of waiting; nil leaves it unpriced.
	Rate *ExtendedSupportRate
}

// DefaultTargetMajor is the brief's target when none is asked for: the
// newest major the provider offers, else the newest community major
// (never a major pg_sage has no dated facts for).
func DefaultTargetMajor(offered int) int {
	newest := pgversion.NewestKnownMajor()
	if offered > 0 && offered <= newest {
		return offered
	}
	return newest
}

// supportState is provider's support of major on now's date: the end
// date is the last supported day.
func supportState(provider string, major int, now time.Time) SupportState {
	out := SupportState{Provider: provider, Major: major, Status: SupportUnknown}
	w, ok := pgversion.Support(provider, major)
	if !ok {
		return out
	}
	out.Ends, out.Source = w.End, w.Source
	end := w.EndTime()
	switch {
	case end.IsZero():
	case now.Before(end.Add(24 * time.Hour)):
		out.Status = SupportSupported
	default:
		out.Status = SupportEnded
	}
	return out
}
