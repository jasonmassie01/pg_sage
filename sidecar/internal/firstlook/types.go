// Package firstlook is pg_sage's catalog-only first look (roadmap phase 3,
// "five-minute time to value"). Within the first minute of a new install
// it reads the system catalog and the statistics views once, in one
// read-only transaction, and reports what a DBA would flag on day one:
// invalid, duplicate, redundant and never-scanned indexes, unindexed
// foreign keys, transaction ID and sequence runway, cheap bloat estimates,
// leftover test schemas (as binding-fact proposals, never actions) and the
// extensions pg_sage would like, with exact enablement steps.
//
// It needs no pg_stat_statements history, no superuser and no LLM. Every
// item cites the catalog evidence it rests on. It never reads user table
// data and never writes outside pg_sage's own schema.
package firstlook

import (
	"errors"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/facts"
)

// Severity of a first-look item.
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// Rule names; each item and check carries one.
const (
	RuleInvalidIndex      = "invalid_index"
	RuleDuplicateIndex    = "duplicate_index"
	RuleRedundantIndex    = "redundant_index"
	RuleNeverScannedIndex = "never_scanned_index"
	RuleUnindexedFK       = "unindexed_foreign_key"
	RuleXIDRunway         = "xid_runway"
	RuleMultiXactRunway   = "multixact_runway"
	RuleSequenceRunway    = "sequence_runway"
	RuleTableBloat        = "table_bloat_estimate"
	RuleTestSchema        = "test_schema"
	RuleMissingExtension  = "missing_extension"
	RuleQueryTextHidden   = "query_text_hidden"
)

// CheckStatus is how one rule's check ended.
type CheckStatus string

// Check statuses.
const (
	CheckOK       CheckStatus = "ok"
	CheckFinding  CheckStatus = "finding"
	CheckDegraded CheckStatus = "degraded"
)

// Errors, each distinguishable with errors.Is.
var (
	ErrNoPool        = errors.New("firstlook: no database connection pool")
	ErrNoDatabase    = errors.New("firstlook: report has no database name")
	ErrNotFound      = errors.New("firstlook: report not found")
	ErrNoModel       = errors.New("firstlook: no model configured")
	ErrSummaryOutput = errors.New("firstlook: model reply is not a summary")
)

// Evidence is one catalog observation an item rests on.
type Evidence struct {
	Source string `json:"source"`
	Ref    string `json:"ref"`
	Detail string `json:"detail,omitempty"`
}

// Item is one first-look finding. SuggestedSQL is for the operator to
// review; pg_sage never runs it.
type Item struct {
	Rule string `json:"rule"`
	// Section groups items in the report; empty is the catalog checks,
	// SectionAgentPosture the agent posture checks.
	Section        string     `json:"section,omitempty"`
	Severity       string     `json:"severity"`
	Object         string     `json:"object"`
	Title          string     `json:"title"`
	Detail         string     `json:"detail,omitempty"`
	Recommendation string     `json:"recommendation,omitempty"`
	SuggestedSQL   string     `json:"suggested_sql,omitempty"`
	Caveat         string     `json:"caveat,omitempty"`
	Evidence       []Evidence `json:"evidence"`
}

// Check is how one rule's catalog check went; a degraded check states why.
type Check struct {
	Rule    string      `json:"rule"`
	Section string      `json:"section,omitempty"`
	Status  CheckStatus `json:"status"`
	Note    string      `json:"note,omitempty"`
	// Retried: the check degraded with a transient error and was run once
	// more; Note keeps the first attempt's failure.
	Retried bool `json:"retried,omitempty"`
}

// Report is one first look of one database.
type Report struct {
	ID                 int64        `json:"id"`
	Database           string       `json:"database"`
	Provider           string       `json:"provider,omitempty"`
	StartedAt          time.Time    `json:"started_at"`
	FinishedAt         time.Time    `json:"finished_at"`
	DurationMS         int64        `json:"duration_ms"`
	Relations          int          `json:"relations"`
	StatementTimeoutMS int          `json:"statement_timeout_ms"`
	Items              []Item       `json:"items"`
	Checks             []Check      `json:"checks"`
	Capabilities       []Capability `json:"capabilities"`
	Summary            string       `json:"summary,omitempty"`
	SummaryModel       string       `json:"summary_model,omitempty"`
	// FactProposals are the test-schema facts the first look proposes; the
	// caller records them in the fact store (an operator confirms them).
	FactProposals []facts.Proposal `json:"-"`
	// Retryable are the rules whose checks degraded with a transient error
	// (a timeout, a lost connection), for Retry.
	Retryable []string `json:"-"`
}

// StatsWindow is how far back the cumulative statistics reach: since the
// last statistics reset, else since the server started.
type StatsWindow struct {
	Since  time.Time
	Source string // "stats_reset" or "server_start"; "" when unknown
	Known  bool
}

// Thresholds tune the rules.
type Thresholds struct {
	XIDWarnFraction          float64
	XIDCriticalFraction      float64
	SequenceWarnFraction     float64
	SequenceCriticalFraction float64
	BloatMinBytes            int64
	BloatDeadFraction        float64
	FKMinRows                float64
	MaxItemsPerRule          int
}

// XIDFraction is a transaction ID age as a share of the distance to
// wraparound, for the analyzer's absolute xid_wraparound thresholds.
func XIDFraction(age int64) float64 { return float64(age) / xidWrapLimit }

// DefaultThresholds are the shipped limits; the XID ones match the
// analyzer's defaults (analyzer.xid_wraparound_warning and _critical).
func DefaultThresholds() Thresholds {
	return Thresholds{
		XIDWarnFraction:          XIDFraction(500_000_000),
		XIDCriticalFraction:      XIDFraction(1_000_000_000),
		SequenceWarnFraction:     0.70,
		SequenceCriticalFraction: 0.90,
		BloatMinBytes:            100 << 20,
		BloatDeadFraction:        0.20,
		FKMinRows:                1000,
		MaxItemsPerRule:          50,
	}
}

var severityRank = map[string]int{SeverityCritical: 0, SeverityWarning: 1, SeverityInfo: 2}

// sortItems orders items by severity, then object, then rule.
func sortItems(items []Item) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if severityRank[a.Severity] != severityRank[b.Severity] {
			return severityRank[a.Severity] < severityRank[b.Severity]
		}
		if a.Object != b.Object {
			return a.Object < b.Object
		}
		return a.Rule < b.Rule
	})
}

// capItems keeps the first max items (sorted) and reports how many were
// dropped.
func capItems(items []Item, max int) ([]Item, int) {
	sortItems(items)
	if max <= 0 || len(items) <= max {
		return items, 0
	}
	return items[:max], len(items) - max
}
