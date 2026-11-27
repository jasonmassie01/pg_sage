package upgradecheck

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// The upgrade rehearsal (drill-02b). Plans flip after a major upgrade
// because statistics are not carried over before PG18. A rehearsal clones
// the database (receipt purpose rehearsal, so a crash leaves a record the
// reaper destroys), captures the plans and blocks per call of the top
// statements on the clone, upgrades the clone (never the source), captures
// again as cut over, runs ANALYZE, captures a third time and destroys the
// clone. The report lists every statement whose plan changed.

// UpgradeRehearser upgrades the rehearsal clone cl to targetMajor and
// returns where it now answers. The result must keep cl's ID: it is the
// receipted resource the rehearsal destroys.
type UpgradeRehearser interface {
	Upgrade(ctx context.Context, cl clone.Clone, targetMajor int) (clone.Clone, error)
}

// Rehearsal states, recorded in order in RehearsalReport.Steps.
const (
	RehearsalRequested = "requested"
	RehearsalCloned    = "cloned"
	RehearsalUpgraded  = "upgraded"
	RehearsalAnalyzed  = "analyzed"
	RehearsalDestroyed = "destroyed"
	RehearsalFailed    = "failed"
)

// Statement statuses in a rehearsal.
const (
	QueryCompared      = "compared"
	QuerySkipped       = "skipped"
	QueryNotComparable = "not_comparable"
	QueryError         = "error"
)

// MaxRehearsalQueries is how many top statements a rehearsal compares.
const MaxRehearsalQueries = 50

// NothingToCompare is the note of a rehearsal with no top statements.
const NothingToCompare = "nothing to compare: production reported no top statements"

// Defaults of a rehearsal's bounds.
const (
	DefaultRehearsalStatementTimeout = 5 * time.Second
	DefaultRehearsalAnalyzeTimeout   = 30 * time.Minute
)

// Rehearsal errors, each distinguishable with errors.Is.
var (
	ErrInvalidRehearsal   = errors.New("upgrade rehearsal: invalid request")
	ErrUpgradeUnsupported = errors.New("upgrade rehearsal: the provider cannot upgrade a clone")
	ErrUpgradeRefused     = errors.New("upgrade rehearsal: upgrade refused")
)

// TopQuery is one of production's top statements.
type TopQuery struct {
	QueryID int64
	Text    string
}

// TopQuerySource reads at most limit of production's top statements,
// most total time first.
type TopQuerySource func(ctx context.Context, limit int) ([]TopQuery, error)

// RehearsalStep is one recorded state of a rehearsal.
type RehearsalStep struct {
	State  string    `json:"state"`
	At     time.Time `json:"at"`
	Detail string    `json:"detail,omitempty"`
}

// PlanCapture is one statement measured on the clone once. BlocksPerCall
// is nil when the statement was planned only (NotExecuted says why).
type PlanCapture struct {
	PlanHash      string  `json:"plan_hash,omitempty"`
	Cost          float64 `json:"cost"`
	BlocksPerCall *int64  `json:"blocks_per_call,omitempty"`
	NotExecuted   string  `json:"not_executed,omitempty"`
	Error         string  `json:"error,omitempty"`
}

// RehearsedQuery is one top statement across the three captures: before
// the upgrade, as cut over (no ANALYZE) and after ANALYZE.
type RehearsedQuery struct {
	QueryID      int64        `json:"queryid"`
	Text         string       `json:"text"`
	Status       string       `json:"status"`
	Reason       string       `json:"reason,omitempty"`
	Before       *PlanCapture `json:"before,omitempty"`
	AsCutOver    *PlanCapture `json:"as_cut_over,omitempty"`
	AfterAnalyze *PlanCapture `json:"after_analyze,omitempty"`

	ChangedAsCutOver        bool   `json:"plan_changed_as_cut_over"`
	ChangedAfterAnalyze     bool   `json:"plan_changed_after_analyze"`
	BlocksDeltaAsCutOver    *int64 `json:"blocks_per_call_delta_as_cut_over,omitempty"`
	BlocksDeltaAfterAnalyze *int64 `json:"blocks_per_call_delta_after_analyze,omitempty"`
}

// PlanChanged reports a plan that differs from before in either capture.
func (q RehearsedQuery) PlanChanged() bool {
	return q.ChangedAsCutOver || q.ChangedAfterAnalyze
}

// RecoveredAfterAnalyze reports a plan that changed as cut over and is
// back to the old one after ANALYZE.
func (q RehearsedQuery) RecoveredAfterAnalyze() bool {
	return q.ChangedAsCutOver && !q.ChangedAfterAnalyze
}

// RehearsalReport is one rehearsal: its steps, the time the upgrade and
// ANALYZE took on the clone, and every top statement whose plan changed.
type RehearsalReport struct {
	Database    string          `json:"database"`
	Provider    string          `json:"provider,omitempty"`
	SourceMajor int             `json:"source_major"`
	TargetMajor int             `json:"target_major"`
	ReceiptID   int64           `json:"receipt_id,omitempty"`
	CloneID     string          `json:"clone_id,omitempty"`
	State       string          `json:"state"`
	Steps       []RehearsalStep `json:"steps"`
	StartedAt   time.Time       `json:"started_at"`
	FinishedAt  time.Time       `json:"finished_at,omitempty"`

	UpgradeAt      time.Time `json:"upgrade_at,omitempty"`
	UpgradeSeconds float64   `json:"upgrade_seconds"`
	AnalyzeAt      time.Time `json:"analyze_at,omitempty"`
	AnalyzeSeconds float64   `json:"analyze_seconds"`

	Changed               []RehearsedQuery `json:"changed"`
	NotCompared           []RehearsedQuery `json:"not_compared"`
	Unchanged             int              `json:"unchanged"`
	PlansChanged          int              `json:"plans_changed"`
	RecoveredAfterAnalyze int              `json:"recovered_after_analyze"`

	Note            string `json:"note,omitempty"`
	Error           string `json:"error,omitempty"`
	ProviderMessage string `json:"provider_message,omitempty"`
	CleanupError    string `json:"cleanup_error,omitempty"`
}

// RehearsalOptions are one rehearsal's inputs. Provider must write
// receipts (a clone.ReceiptingProvider of purpose rehearsal, which also
// applies the cost label and the full-copy opt-in); Production is the
// monitored server, so a clone is never connected when it is production.
type RehearsalOptions struct {
	Database     string
	SourceMajor  int
	TargetMajor  int
	Provider     clone.Provider
	ProviderName string
	Upgrader     UpgradeRehearser
	Production   clone.IdentitySource
	Top          TopQuerySource
	// StatementTimeout bounds each measured statement (0: the default).
	StatementTimeout time.Duration
	// AnalyzeTimeout bounds the ANALYZE on the clone (0: the default).
	AnalyzeTimeout time.Duration
	// Inspect is the analyze guard's parser (nil: libpg_query).
	Inspect func(string) (sqlast.ReadQuery, error)
	// OnStep sees the report after every recorded step (progress).
	OnStep func(RehearsalReport)
}

func (o RehearsalOptions) validate() error {
	var why string
	switch {
	case o.Database == "":
		why = "a database is required"
	case o.Provider == nil || o.Upgrader == nil || o.Production == nil || o.Top == nil:
		why = "a clone provider, an upgrader, the production identity and a top " +
			"statement source are required"
	case o.SourceMajor < 1 || o.TargetMajor > 1000:
		why = fmt.Sprintf("majors %d and %d are not PostgreSQL majors", o.SourceMajor,
			o.TargetMajor)
	case o.TargetMajor < o.SourceMajor:
		why = fmt.Sprintf("target %d is below the source %d", o.TargetMajor, o.SourceMajor)
	case o.StatementTimeout < 0 || o.AnalyzeTimeout < 0:
		why = "timeouts must not be negative"
	}
	if why == "" {
		if _, ok := o.Provider.(clone.ReceiptNamer); !ok {
			why = "the clone provider writes no receipts, so a crash would leave a clone " +
				"no reaper finds"
		}
	}
	if why != "" {
		return fmt.Errorf("%w: %s", ErrInvalidRehearsal, why)
	}
	return nil
}

func (o RehearsalOptions) withDefaults() RehearsalOptions {
	if o.StatementTimeout == 0 {
		o.StatementTimeout = DefaultRehearsalStatementTimeout
	}
	if o.AnalyzeTimeout == 0 {
		o.AnalyzeTimeout = DefaultRehearsalAnalyzeTimeout
	}
	return o
}
