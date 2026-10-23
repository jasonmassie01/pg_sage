package policy

import (
	"fmt"
	"time"
)

// BudgetKind separates the blast-radius budgets, so housekeeping can never
// spend the budget evidence-backed performance changes need (dogfood
// lifeos, 2026-10-03: redundant-index drops in leaked test schemas held
// two verified indexes back for most of a day).
type BudgetKind string

const (
	// BudgetPerformance is every change that is not hygiene, including any
	// action type pg_sage does not know (fail closed).
	BudgetPerformance BudgetKind = "performance"
	// BudgetHygiene is housekeeping: unused and redundant index drops
	// (including those in leaked schemas), VACUUM and ANALYZE.
	BudgetHygiene BudgetKind = "hygiene"
)

// BudgetWindow is the rolling window every blast-radius limit counts over.
const BudgetWindow = 24 * time.Hour

// The profiles split today's envelope (20 tables and 50 self-initiated
// changes per window) evenly between the kinds. A stored document that
// predates the split keeps its single limit for performance, and hygiene
// takes the hygiene defaults.
const (
	DefaultPerformanceTablesPerWindow  = 10
	DefaultPerformanceChangesPerWindow = 25
	DefaultHygieneTablesPerWindow      = 10
	DefaultHygieneChangesPerWindow     = 25
)

// KindBudget bounds one kind of change per window: distinct tables touched
// and self-initiated changes. A zero table limit admits no table.
type KindBudget struct {
	MaxTablesPerWindow  int64 `json:"max_tables_per_window"`
	MaxChangesPerWindow int64 `json:"max_changes_per_window"`
}

// DefaultHygieneBudget is the hygiene budget of a document that sets none.
func DefaultHygieneBudget() KindBudget {
	return KindBudget{
		MaxTablesPerWindow:  DefaultHygieneTablesPerWindow,
		MaxChangesPerWindow: DefaultHygieneChangesPerWindow,
	}
}

// BudgetKindFor classifies a request by its typed action contract alone:
// never by evidence, the feature a caller names or an LLM's output. A
// request without a contract, or with an action type not listed here, is
// performance (fail closed).
func BudgetKindFor(req ActionRequest) BudgetKind {
	if req.Contract == nil {
		return BudgetPerformance
	}
	switch req.Contract.ActionType {
	case "drop_unused_index", "vacuum_table", "analyze_table":
		return BudgetHygiene
	default:
		return BudgetPerformance
	}
}

// Budget is the document's budget for kind; an unknown kind reads the
// performance budget. The performance budget keeps its legacy fields,
// blast_radius.max_tables_per_window and
// rate_limits.max_self_initiated_changes_per_window.
func (doc Document) Budget(kind BudgetKind) KindBudget {
	if kind == BudgetHygiene {
		return doc.BlastRadius.Hygiene
	}
	return KindBudget{
		MaxTablesPerWindow:  doc.BlastRadius.MaxTablesPerWindow,
		MaxChangesPerWindow: doc.RateLimits.MaxSelfInitiatedChangesPerWindow,
	}
}

// spendsBudget reports a request the usage limits apply to: a
// self-initiated mutation. Authorizations of such requests are serialized
// so two candidates cannot both take the last slot of a budget.
func spendsBudget(req ActionRequest) bool {
	return !req.OperatorApproved && req.Contract != nil &&
		req.Contract.RiskTier != RiskReadOnly && !req.Contract.Narrowing
}

// budgetNote is what the gate learned from usage, stamped on the decision
// so the ledger charges an execution to its kind and rows.
type budgetNote struct {
	read bool
	kind BudgetKind
	rows int64
	// bypass is why the kind budgets did not apply (BudgetBypassFor).
	bypass string
}

func (note budgetNote) stamp(decision Decision) Decision {
	if note.read {
		decision.BudgetKind = note.kind
		decision.RowsRewritten = note.rows
	}
	if note.bypass != "" {
		decision.BudgetKind = BudgetBypass
		if decision.Detail == "" {
			decision.Detail = "budget bypass: " + note.bypass
		}
	}
	return decision
}

// limitDecision checks the shared bounds (storage, rows rewritten) and,
// unless the request bypasses them, the kind budget.
func limitDecision(
	doc Document, kind BudgetKind, usage LimitUsage, bypass bool,
) (Decision, bool) {
	if budgetExceeded(doc.Budgets.StorageBytes, usage.StorageBytes) {
		return blockedAs(VerdictPark, ReasonBudgetExceeded), true
	}
	if positiveExceeded(doc.BlastRadius.MaxRowsRewritten, usage.RowsRewritten, false) {
		return parked(ReasonBlastRadiusExceeded, rowsDetail(doc, usage)), true
	}
	if bypass {
		return Decision{}, false
	}
	budget := doc.Budget(kind)
	if positiveExceeded(budget.MaxTablesPerWindow, usage.TablesInWindow, false) {
		return parked(ReasonBlastRadiusExceeded, kindDetail(kind, "tables",
			usage.TablesInWindow, budget.MaxTablesPerWindow, usage.TablesFreeAt)), true
	}
	changes := usage.SelfInitiatedChangesInWindow
	if positiveExceeded(budget.MaxChangesPerWindow, changes, true) {
		return parked(ReasonRateLimitExceeded, kindDetail(kind, "changes",
			changes, budget.MaxChangesPerWindow, usage.ChangesFreeAt)), true
	}
	return Decision{}, false
}

func parked(reason Reason, detail string) Decision {
	return Decision{Verdict: VerdictPark, Reason: reason, Detail: detail}
}

// kindDetail says which budget is full, how full, and when it frees.
func kindDetail(kind BudgetKind, unit string, used, limit int64, freeAt time.Time) string {
	return fmt.Sprintf("%s budget full: %d of %d %s in the %s window; %s",
		kind, used, limit, unit, windowName(), freesAt(freeAt))
}

func rowsDetail(doc Document, usage LimitUsage) string {
	limit := doc.BlastRadius.MaxRowsRewritten
	free := freesAt(usage.RowsFreeAt)
	if usage.RequestRowsRewritten > limit {
		free = "this change alone exceeds the limit; " + freesAt(time.Time{})
	}
	return fmt.Sprintf("rows rewritten budget full (shared by all kinds): %d of %d rows "+
		"in the %s window, %d of them this change's; %s",
		usage.RowsRewritten, limit, windowName(), usage.RequestRowsRewritten, free)
}

func freesAt(at time.Time) string {
	if at.IsZero() {
		return "it does not free until the policy limit is raised"
	}
	return "next frees at " + at.UTC().Format(time.RFC3339)
}

func windowName() string {
	return fmt.Sprintf("%.0fh", BudgetWindow.Hours())
}

func budgetExceeded(limit BudgetLimit, usage int64) bool {
	if limit.NoCap() {
		return false
	}
	value, ok := limit.Value()
	if !ok {
		return true
	}
	return value == 0 || usage > value
}

func positiveExceeded(limit, usage int64, includeEqual bool) bool {
	if limit == 0 {
		return usage > 0
	}
	if includeEqual {
		return usage >= limit
	}
	return usage > limit
}

// BudgetBypass is the kind recorded for an emergency mitigation that the
// kind budgets do not bind (BudgetBypassFor). It is charged to no kind.
const BudgetBypass BudgetKind = "bypass"

// BudgetBypassFor names why a request is an emergency mitigation that no
// kind budget may park, or returns "". It is decided from the typed
// contract and the deadline the custodian computed from its runway (owner
// decision 2026-10-03), never from evidence or LLM text:
//
//   - a VACUUM (wraparound freeze) when the XID runway is critical;
//   - a space-freeing action (VACUUM, unused index drop, REINDEX) when the
//     disk runway is critical;
//   - the revert or rollback of a change pg_sage made itself.
//
// Critical means urgency critical with a hard deadline still ahead. Every
// other gate check still applies, and so does the shared rows bound.
func BudgetBypassFor(req ActionRequest, now time.Time) string {
	if req.Contract == nil {
		return ""
	}
	action := req.Contract.ActionType
	switch {
	case action == "revert_created_index":
		return "revert of an index pg_sage created"
	case req.Rollback:
		return "rollback of pg_sage's own change"
	}
	deadline := req.Deadline
	if deadline == nil || deadline.Urgency != UrgencyCritical || !deadline.HardAt.After(now) {
		return ""
	}
	hardAt := deadline.HardAt.UTC().Format(time.RFC3339)
	switch {
	case deadline.Kind == DeadlineXID && action == "vacuum_table":
		return "xid runway critical, wraparound hard deadline " + hardAt
	case deadline.Kind == DeadlineDisk && diskMitigation(action):
		return "disk runway critical, hard deadline " + hardAt
	}
	return ""
}

func diskMitigation(actionType string) bool {
	switch actionType {
	case "vacuum_table", "drop_unused_index", "reindex_concurrently":
		return true
	default:
		return false
	}
}
