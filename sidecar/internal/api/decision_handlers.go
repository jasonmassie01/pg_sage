package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Reading the decision ledger (policy-07). GET /api/v1/decisions answers
// "why hasn't this run?" for every withheld candidate of the selected
// databases; GET /api/v1/policy/explain (policy_explain.go) answers it
// for one finding before the gate has recorded anything. Both are
// operator routes: a viewer sees no decision data. Neither returns SQL
// text, and neither records anything.

const decisionsPath = "/api/v1/decisions"

// decisionsDatabaseTimeout bounds one database's read, so one slow or
// unreachable database cannot hold the fleet view.
var decisionsDatabaseTimeout = 5 * time.Second

// readDatabaseDecisions is one database's read; a seam for tests.
var readDatabaseDecisions = func(
	ctx context.Context, sel namedPool, filter ledger.WithheldFilter,
) ([]ledger.WithheldEntry, bool, error) {
	return ledger.ListOpenWithheld(ctx, sel.pool, filter)
}

func registerDecisionRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	routeAggregate(mux, "GET "+decisionsPath, gateOperator, mgr, aggregateHandler(decisionsHandler))
	routeAggregate(mux, "GET /api/v1/policy/explain", gateOperator, mgr,
		aggregateHandler(policyExplainHandler))
}

// decisionEntry is one candidate's open withheld decisions as served.
type decisionEntry struct {
	Database string `json:"database"`
	ledger.WithheldEntry
	HumanReason string `json:"human_reason"`
}

// decisionsUnavailable names a database whose ledger could not be read,
// by error class (raw errors can carry hosts and roles).
type decisionsUnavailable struct {
	Database string `json:"database"`
	Error    string `json:"error"`
}

type decisionsResponse struct {
	Database  string                 `json:"database"`
	Decisions []decisionEntry        `json:"decisions"`
	Partial   []decisionsUnavailable `json:"partial"`
	Truncated bool                   `json:"truncated"`
	Limit     int                    `json:"limit"`
}

func decisionsHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		filter, err := parseDecisionsFilter(r)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if rejectUnknownDatabase(w, mgr, database) {
			return
		}
		out := collectDecisions(r.Context(), mgr, database, filter)
		status := http.StatusOK
		if len(out.Decisions) == 0 && len(out.Partial) > 0 &&
			len(out.Partial) == selectedDatabaseCount(mgr, database) {
			status = http.StatusServiceUnavailable
		}
		writeJSONStatus(w, status, out)
	}
}

// parseDecisionsFilter reads verdict, reason, since and limit; each error
// names its parameter.
func parseDecisionsFilter(r *http.Request) (ledger.WithheldFilter, error) {
	q := r.URL.Query()
	filter := ledger.WithheldFilter{Verdict: ledger.Verdict(q.Get("verdict")),
		Reason: q.Get("reason"), Limit: ledger.DefaultWithheldLimit}
	if filter.Verdict != "" && !ledger.IsWithheldVerdict(filter.Verdict) {
		return filter, errors.New("verdict must be one of queue_approval, parked, " +
			"blocked, observe_only")
	}
	if filter.Reason != "" && !policy.IsLedgerReason(filter.Reason) {
		return filter, errors.New("reason must be a policy gate reason " +
			"(docs/policy-gate.md#reasons)")
	}
	if raw := q.Get("since"); raw != "" {
		since, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return filter, errors.New("since must be an RFC 3339 time")
		}
		filter.Since = &since
	}
	if raw := q.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > ledger.MaxWithheldLimit {
			return filter, errors.New("limit must be an integer from 1 to 500")
		}
		filter.Limit = limit
	}
	return filter, nil
}

// collectDecisions reads every selected database concurrently, each under
// its own deadline, and merges the entries newest first, cut at the limit.
func collectDecisions(
	ctx context.Context, mgr *fleet.DatabaseManager, database string,
	filter ledger.WithheldFilter,
) decisionsResponse {
	selected := poolsForDatabaseSelection(mgr, database)
	results := make([][]ledger.WithheldEntry, len(selected))
	cut := make([]bool, len(selected))
	errs := make([]error, len(selected))
	slots := make(chan struct{}, casesMaxConcurrentDatabases)
	var wg sync.WaitGroup
	for i, sel := range selected {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			dbCtx, cancel := context.WithTimeout(ctx, decisionsDatabaseTimeout)
			defer cancel()
			results[i], cut[i], errs[i] = readDatabaseDecisions(dbCtx, sel, filter)
		}()
	}
	wg.Wait()
	out := mergeDecisions(selected, results, cut, errs, filter.Limit)
	out.Database = responseDatabaseName(database)
	out.Partial = append(out.Partial, unpooledDecisionDatabases(mgr, database, selected)...)
	return out
}

func mergeDecisions(
	selected []namedPool, results [][]ledger.WithheldEntry, cut []bool, errs []error,
	limit int,
) decisionsResponse {
	out := decisionsResponse{Decisions: []decisionEntry{},
		Partial: []decisionsUnavailable{}, Limit: limit}
	for i, sel := range selected {
		if errs[i] != nil {
			class := classifyCasesError(errs[i])
			slog.Warn("decisions read failed for database", "database", sel.name,
				"error_class", class, "error", errs[i])
			out.Partial = append(out.Partial,
				decisionsUnavailable{Database: sel.name, Error: class})
			continue
		}
		out.Truncated = out.Truncated || cut[i]
		for _, entry := range results[i] {
			out.Decisions = append(out.Decisions, decisionEntry{Database: sel.name,
				WithheldEntry: entry, HumanReason: entryHumanReason(entry)})
		}
	}
	sort.SliceStable(out.Decisions, func(a, b int) bool {
		return out.Decisions[a].LastSeenAt.After(out.Decisions[b].LastSeenAt)
	})
	if len(out.Decisions) > limit {
		out.Decisions, out.Truncated = out.Decisions[:limit], true
	}
	return out
}

// entryHumanReason is the reason as the gate's surfaces render it, with
// the budget's or the verification wait's detail.
func entryHumanReason(entry ledger.WithheldEntry) string {
	detail := entry.BudgetDetail
	if detail == "" {
		detail = entry.VerificationWaitDetail
	}
	return executor.HumanDecisionReason(entry.Reason, detail)
}

// unpooledDecisionDatabases names the selected databases that have no
// pool (down when the sidecar started) as unavailable.
func unpooledDecisionDatabases(
	mgr *fleet.DatabaseManager, database string, selected []namedPool,
) []decisionsUnavailable {
	if mgr == nil {
		return nil
	}
	read := make(map[string]bool, len(selected))
	for _, sel := range selected {
		read[sel.name] = true
	}
	var out []decisionsUnavailable
	for name, inst := range mgr.Instances() {
		if inst == nil || inst.Pool != nil || read[name] {
			continue
		}
		if database == "" || database == "all" || name == database {
			out = append(out, decisionsUnavailable{Database: name, Error: caseErrorConnection})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Database < out[b].Database })
	return out
}

// selectedDatabaseCount is how many databases the selection names.
func selectedDatabaseCount(mgr *fleet.DatabaseManager, database string) int {
	if mgr == nil {
		return 0
	}
	if database != "" && database != "all" {
		return 1
	}
	return len(mgr.Instances())
}
