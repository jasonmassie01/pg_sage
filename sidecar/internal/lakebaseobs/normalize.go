package lakebaseobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

var ErrSelfStatement = errors.New("lakebase telemetry is pg_sage monitoring")

// Plan preserves capture provenance. Nil flags mean provider did not supply the
// flag; they must not become false or be inferred from the capture source.
type Plan struct {
	QueryID        int64
	Query          string
	JSON           []byte
	TotalCost      float64
	ExecutionMS    float64
	CapturedAt     time.Time
	AnalyzeEnabled *bool
	BuffersEnabled *bool
	TimingEnabled  *bool
	Fingerprint    string
	DurationSource string
}

func NormalizePlan(e Event) (Plan, error) {
	var p Plan
	if e.Stream != Plans || e.OccurredAt.IsZero() {
		return p, ErrMalformed
	}
	f := e.Fields
	if selfmonitor.IsApplicationName(f["application_name"]) || selfmonitor.IsQueryText(f["query_text"]) {
		return p, ErrSelfStatement
	}
	if !e.PayloadAllowed || missing(f["plan_json"]) || missing(f["query_text"]) {
		return p, ErrMasked
	}
	truncated, err := optionalBool(f["truncated"])
	if err != nil || truncated == nil || *truncated {
		return p, ErrMalformed
	}
	if len(f["plan_json"]) > 1<<20 || len(f["query_text"]) > 1<<20 {
		return p, ErrBudget
	}
	p.QueryID, err = queryID(f["queryid"])
	if err != nil || p.QueryID == 0 {
		return Plan{}, ErrMalformed
	}
	p.AnalyzeEnabled, err = optionalBool(f["analyze_enabled"])
	if err != nil {
		return Plan{}, err
	}
	p.BuffersEnabled, err = optionalBool(f["buffers_enabled"])
	if err != nil {
		return Plan{}, err
	}
	p.TimingEnabled, err = optionalBool(f["timing_enabled"])
	if err != nil {
		return Plan{}, err
	}
	body := bytes.TrimSpace([]byte(f["plan_json"]))
	var wrapper []json.RawMessage
	if len(body) == 0 {
		return Plan{}, ErrMalformed
	}
	if body[0] == '{' {
		wrapper = []json.RawMessage{body}
	} else if json.Unmarshal(body, &wrapper) != nil || len(wrapper) != 1 {
		return Plan{}, ErrMalformed
	}
	var doc struct {
		QueryID json.Number `json:"Query Identifier"`
		Query   string      `json:"Query Text"`
		Plan    *struct {
			NodeType   string   `json:"Node Type"`
			TotalCost  *float64 `json:"Total Cost"`
			ActualRows *float64 `json:"Actual Rows"`
		} `json:"Plan"`
	}
	if json.Unmarshal(wrapper[0], &doc) != nil || doc.Plan == nil || doc.Plan.NodeType == "" || doc.Plan.TotalCost == nil || !finite(*doc.Plan.TotalCost) {
		return Plan{}, ErrMalformed
	}
	if selfmonitor.IsQueryText(doc.Query) {
		return Plan{}, ErrSelfStatement
	}
	if doc.QueryID != "" {
		id, idErr := queryID(string(doc.QueryID))
		if idErr != nil || id != p.QueryID {
			return Plan{}, ErrMalformed
		}
	}
	if doc.Plan.ActualRows != nil && !finite(*doc.Plan.ActualRows) {
		return Plan{}, ErrMalformed
	}
	if p.AnalyzeEnabled != nil && *p.AnalyzeEnabled && doc.Plan.ActualRows == nil {
		return Plan{}, ErrMalformed
	}
	p.TotalCost = *doc.Plan.TotalCost
	if value := f["total_cost"]; value != "" {
		if _, err = number(value); err != nil {
			return Plan{}, err
		}
	}
	p.ExecutionMS, err = number(f["duration_ms"])
	if err != nil {
		return Plan{}, err
	}
	p.JSON, err = json.Marshal(wrapper)
	if err != nil {
		return Plan{}, ErrMalformed
	}
	p.Query = f["query_text"]
	p.CapturedAt = e.OccurredAt
	p.Fingerprint = f["plan_fingerprint"]
	p.DurationSource = "lakebase_plan_history.duration_ms"
	return p, nil
}

func NormalizeLog(e Event) (logwatch.LogEntry, error) {
	var out logwatch.LogEntry
	if e.Stream != Logs || e.OccurredAt.IsZero() {
		return out, ErrMalformed
	}
	f := e.Fields
	if selfmonitor.IsApplicationName(f["application_name"]) || selfmonitor.IsQueryText(f["statement"]) {
		return out, ErrSelfStatement
	}
	if !e.PayloadAllowed || missing(f["message"]) {
		return out, ErrMasked
	}
	trunc, err := strconv.ParseInt(f["truncated_mask"], 10, 64)
	if err != nil || trunc != 0 {
		return out, ErrMalformed
	}
	pid, err := strconv.Atoi(f["backend_pid"])
	if err != nil || pid <= 0 {
		return out, ErrMalformed
	}
	if len(f["message"])+len(f["detail"])+len(f["hint"])+len(f["statement"]) > 1<<20 {
		return out, ErrBudget
	}
	if len(f["sqlstate"]) != 0 && len(f["sqlstate"]) != 5 {
		return out, ErrMalformed
	}
	// The provider does not supply backend start/session ID. PID+compute alone
	// can be reused, so leave SessionID unknown rather than inventing identity.
	out = logwatch.LogEntry{Timestamp: e.OccurredAt, PID: pid, Database: f["datname"], User: f["usename"], ErrorLevel: f["severity"], SQLState: f["sqlstate"], Message: f["message"], Detail: f["detail"], Hint: f["hint"], Query: f["statement"], Application: f["application_name"]}
	return out, nil
}

type SessionSample struct {
	At                              time.Time
	ComputeID                       string
	PID, BlockingPID, LeaderPID     int64
	QueryID                         int64
	WaitType, Wait, PlanFingerprint string
	TransactionAgeMS, QueryAgeMS    *float64
}

func NormalizeASH(e Event) (SessionSample, error) {
	var s SessionSample
	if e.Stream != ASH {
		return s, ErrMalformed
	}
	f := e.Fields
	pid, err := integer(f["pid"])
	if err != nil || pid <= 0 {
		return s, ErrMalformed
	}
	blocking, err := optionalInteger(f["blocking_pid"])
	if err != nil || blocking < 0 {
		return s, ErrMalformed
	}
	leader, err := optionalInteger(f["leader_pid"])
	if err != nil || leader < 0 {
		return s, ErrMalformed
	}
	q, err := queryID(f["queryid"])
	if err != nil {
		return s, err
	}
	x, err := optionalNumber(f["xact_age_ms"])
	if err != nil {
		return s, err
	}
	age, err := optionalNumber(f["query_age_ms"])
	if err != nil {
		return s, err
	}
	return SessionSample{e.OccurredAt, e.ComputeID, pid, blocking, leader, q, f["wait_event_type"], f["wait_event"], f["plan_fingerprint"], x, age}, nil
}

// DDLChange intentionally contains no raw SQL or parsed statement payload.
type DDLChange struct {
	At                                                                     time.Time
	ComputeID, Command, ObjectType, ObjectName, Schema, Actor, Application string
}

func NormalizeDDL(e Event) (DDLChange, error) {
	if e.Stream != DDL || e.Fields["command_tag"] == "" {
		return DDLChange{}, ErrMalformed
	}
	f := e.Fields
	return DDLChange{e.OccurredAt, e.ComputeID, f["command_tag"], f["object_type"], f["object_name"], f["schema_name"], f["usename"], f["application_name"]}, nil
}

// MetricInterval is historical only. Counter values already describe [From,To)
// and must not be differenced again as lifetime cumulative counters.
type MetricInterval struct {
	Stream         Stream
	ComputeID      string
	From, To       time.Time
	Counters       bool
	Values         map[string]float64
	HistoricalOnly bool
}

func NormalizeMetrics(e Event) (MetricInterval, error) {
	m := MetricInterval{Stream: e.Stream, ComputeID: e.ComputeID, To: e.OccurredAt, Values: map[string]float64{}, HistoricalOnly: true}
	switch e.Stream {
	case ComputeCounters, DatabaseCounters, WaitCounters, StatementCounters:
		m.Counters = true
	case ComputeGauges, DatabaseGauges:
	default:
		return m, ErrMalformed
	}
	if m.Counters {
		var err error
		m.From, err = parseTime(e.Fields["previous_ts"])
		if err != nil || !m.To.After(m.From) {
			return m, ErrMalformed
		}
	}
	for _, k := range metricFields[e.Stream] {
		if v := e.Fields[k]; v != "" {
			n, err := number(v)
			if err != nil {
				return m, err
			}
			m.Values[k] = n
		}
	}
	return m, nil
}

var metricFields = map[Stream][]string{
	ComputeCounters:   strings.Fields("host_cpu_seconds host_busiest_cpu_seconds host_disk_read_bytes host_disk_written_bytes backpressure_throttling_seconds"),
	ComputeGauges:     strings.Fields("host_logical_cpus host_memory_total_bytes max_connections lfc_size_limit lfc_used total_size_limit total_size replication_lag_time_ms"),
	DatabaseCounters:  strings.Fields("xact_commit xact_rollback deadlocks rows_inserted rows_updated rows_deleted sessions_established sessions_fatal"),
	DatabaseGauges:    strings.Fields("numbackends size oldest_frozen_xid_age oldest_mxid_age"),
	WaitCounters:      strings.Fields("wait_event_count wait_event_time"),
	StatementCounters: strings.Fields("calls total_exec_time total_plan_time rows"),
}

func missing(s string) bool {
	return strings.TrimSpace(s) == "" || strings.EqualFold(strings.TrimSpace(s), "<REDACTED>")
}
func optionalBool(s string) (*bool, error) {
	if s == "" {
		return nil, nil
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		return nil, ErrMalformed
	}
	return &v, nil
}
func number(s string) (float64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || !finite(v) {
		return 0, ErrMalformed
	}
	return v, nil
}
func optionalNumber(s string) (*float64, error) {
	if s == "" {
		return nil, nil
	}
	v, e := number(s)
	return &v, e
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }
func integer(s string) (int64, error) {
	v, e := strconv.ParseInt(s, 10, 64)
	if e != nil {
		return 0, ErrMalformed
	}
	return v, nil
}
func optionalInteger(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	return integer(s)
}
func queryID(s string) (int64, error) {
	if v, e := strconv.ParseInt(s, 10, 64); e == nil {
		return v, nil
	}
	v, e := strconv.ParseUint(s, 10, 64)
	if e != nil {
		return 0, ErrMalformed
	}
	return int64(v), nil
}
