package autoexplain

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/pg-sage/sidecar/internal/logwatch"
)

// Reasons an auto_explain plan record was not stored (rca-04).
const (
	RejectSelf           = "pg_sage_self"
	RejectInvalidPlan    = "invalid_json_plan"
	RejectMissingQueryID = "missing_query_identifier"
	RejectOtherDatabase  = "other_database"
	RejectOverCycleCap   = "over_cycle_cap"
	RejectStoreFailed    = "store_failed"
)

// observedPlanStoreTimeout bounds one cycle's stores on the metadata store.
const observedPlanStoreTimeout = 15 * time.Second

var planMessageHeader = regexp.MustCompile(`^duration:\s+\S+\s+ms\s+plan:`)

// IsPlanMessage reports whether a log message is an auto_explain plan
// record ("duration: <ms> ms  plan:"), as opposed to a plain duration log.
func IsPlanMessage(message string) bool {
	return planMessageHeader.MatchString(message)
}

// ObservedPlanSink stores the executed plans auto_explain writes to the
// server log, as auto_explain_log rows. The Supabase log sink and the
// self-hosted log watcher share it. A value keeps no state between Ingest
// calls: callers keep their own totals from each IngestResult.
type ObservedPlanSink struct {
	Database string // the PostgreSQL database whose plans are kept
	Store    func(context.Context, ObservedPlan) error
	// MaxPerCycle caps the plans one Ingest stores; 0 means no cap.
	MaxPerCycle int
	// StopOnStoreError ends the batch at the first store failure and
	// returns it, so the caller can retry the batch (the Supabase sink).
	// Otherwise the failure is logged with its queryid and counted.
	StopOnStoreError bool
	LogFn            func(level, format string, args ...any)
}

// IngestResult is what one Ingest did: plans stored, and records that look
// like plans but were not stored, by reason.
type IngestResult struct {
	Stored   int
	Rejected map[string]int
}

func (r *IngestResult) reject(reason string) {
	if r.Rejected == nil {
		r.Rejected = make(map[string]int)
	}
	r.Rejected[reason]++
}

// Ingest stores the executed plans among entries. Entries that are not
// plan records are skipped without counting. The stores share one bounded
// context. A cancelled ctx ends the batch with its error.
func (s ObservedPlanSink) Ingest(ctx context.Context, entries []logwatch.LogEntry) (
	IngestResult, error) {
	var res IngestResult
	if s.Database == "" || s.Store == nil {
		return res, errors.New("observed plan sink requires a database and a store")
	}
	if len(entries) == 0 {
		return res, nil
	}
	bounded, cancel := context.WithTimeout(ctx, observedPlanStoreTimeout)
	defer cancel()
	attempts := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		plan, reason := s.parse(entry)
		if reason != "" {
			res.reject(reason)
			continue
		}
		if plan == nil {
			continue
		}
		if s.MaxPerCycle > 0 && attempts >= s.MaxPerCycle {
			res.reject(RejectOverCycleCap)
			continue
		}
		attempts++
		if err := s.store(bounded, *plan, &res); err != nil {
			return res, err
		}
	}
	return res, nil
}

// parse returns entry's plan, or the reason it is rejected; both are empty
// when entry is not a plan record.
func (s ObservedPlanSink) parse(entry logwatch.LogEntry) (*ObservedPlan, string) {
	if !IsPlanMessage(entry.Message) {
		return nil, ""
	}
	if entry.Database != s.Database {
		return nil, RejectOtherDatabase
	}
	plan, err := ParseObservedPlan(entry, s.Database)
	switch {
	case errors.Is(err, ErrSelfStatement):
		return nil, RejectSelf
	case err != nil:
		return nil, RejectInvalidPlan
	case plan.QueryID == 0:
		return nil, RejectMissingQueryID
	}
	return &plan, ""
}

// store stores one plan; a failure is returned only with StopOnStoreError.
func (s ObservedPlanSink) store(ctx context.Context, plan ObservedPlan, res *IngestResult) error {
	err := s.Store(ctx, plan)
	if err == nil {
		res.Stored++
		return nil
	}
	if s.StopOnStoreError {
		return fmt.Errorf("store auto_explain plan queryid=%d: %w", plan.QueryID, err)
	}
	res.reject(RejectStoreFailed)
	if s.LogFn != nil {
		s.LogFn("WARN", "auto_explain log: store plan queryid=%d for database %q: %v",
			plan.QueryID, s.Database, err)
	}
	return nil
}
