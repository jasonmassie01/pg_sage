package probes

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// Historical sources are fixed trusted adapters, not SQL catalog probes.
const (
	LakebasePlans    ID = "lakebase_plan_history"
	LakebaseLogs     ID = "lakebase_log_history"
	LakebaseSessions ID = "lakebase_session_history"
	LakebaseChanges  ID = "lakebase_ddl_history"
	LakebaseDatabase ID = "lakebase_database_history"
)

func HistoricalIDs() []ID {
	return []ID{LakebasePlans, LakebaseLogs, LakebaseSessions, LakebaseChanges, LakebaseDatabase}
}

type HistoricalSource interface {
	ReadHistory(context.Context, ID, Args) Result
}

// HistoricalWindow describes observations, never provider completeness or a
// live counter baseline. No raw SQL/log payload is retained in the result.
type HistoricalWindow struct {
	ScopeHash         string    `json:"scope_hash"`
	From              time.Time `json:"from"`
	To                time.Time `json:"to"`
	RetainedFrom      time.Time `json:"retained_from"`
	Complete          bool      `json:"complete"`
	ExactMultiplicity bool      `json:"exact_multiplicity"`
	LastPoll          time.Time `json:"last_poll"`
	QueryThrough      time.Time `json:"query_through"`
	GapCount          int64     `json:"gap_count"`
	RejectedRows      int       `json:"rejected_rows"`
	UnavailableRows   int       `json:"unavailable_rows"`
	SelfExcludedRows  int       `json:"self_excluded_rows"`
}

func CheckHistoricalArgs(id ID, args Args) error {
	query := id == LakebasePlans || id == LakebaseSessions
	known := query || id == LakebaseLogs || id == LakebaseChanges || id == LakebaseDatabase
	if !known || args.QueryID != 0 && !query {
		return fmt.Errorf("historical source does not accept these arguments")
	}
	args.QueryID = 0
	return args.validate(ArgsWindow)
}

// HistoricalRow is a structured observation at its original provider event
// time. It is deliberately separate from live probe/counter row contracts.
type HistoricalRow struct {
	EventID, Source, Signal, Severity, Command, ObjectType, PlanHash string
	At                                                               time.Time
	QueryID                                                          int64
}

func HistoricalRows(res Result) ([]HistoricalRow, error) {
	if !slices.Contains(HistoricalIDs(), res.ProbeID) || !res.Status.Usable() || res.History == nil {
		return nil, fmt.Errorf("historical evidence unavailable")
	}
	h := res.History
	if h.ScopeHash == "" || h.From.IsZero() || !h.To.After(h.From) || h.Complete || h.ExactMultiplicity {
		return nil, fmt.Errorf("historical evidence has invalid partial-window provenance")
	}
	expected := map[ID][]string{LakebasePlans: {"plan_history"}, LakebaseLogs: {"postgres_logs"}, LakebaseSessions: {"active_session_history"},
		LakebaseChanges: {"ddl_history"}, LakebaseDatabase: {"database_counters", "database_gauges"}}[res.ProbeID]
	out := make([]HistoricalRow, 0, len(res.Rows))
	for _, r := range res.Rows {
		at := timeField(r, "occurred_at")
		if strField(r, "event_id") == "" || strField(r, "scope_hash") != h.ScopeHash ||
			!slices.Contains(expected, strField(r, "source")) || !boolField(r, "historical_only") ||
			boolField(r, "complete") || boolField(r, "exact_multiplicity") || at.Before(h.From) || !at.Before(h.To) || at.Before(h.RetainedFrom) {
			return nil, fmt.Errorf("historical row provenance invalid")
		}
		qid, _ := intField(r, "queryid")
		out = append(out, HistoricalRow{EventID: strField(r, "event_id"), Source: strField(r, "source"), At: at,
			Signal: strField(r, "rca_signal"), Severity: strField(r, "rca_severity"), Command: strField(r, "command"),
			ObjectType: strField(r, "object_type"), PlanHash: strField(r, "plan_hash"), QueryID: qid})
	}
	return out, nil
}
