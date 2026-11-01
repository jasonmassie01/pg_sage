package lakebaseobs

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/planhash"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

const (
	evidenceRows            = 100
	evidenceReadTimeout     = 5 * time.Second
	evidenceRollbackTimeout = 3 * time.Second
)

// EvidenceSource exposes already persisted, SDK-verified database history. It
// copies only structured metadata into SRE evidence: never raw SQL, plans, log
// messages, actors or object names, which would outlive payload retention.
type EvidenceSource struct {
	store     *Store
	runtime   *Runtime
	monitored *pgxpool.Pool
}

func NewEvidenceSource(store *Store, runtime *Runtime, monitored *pgxpool.Pool) (*EvidenceSource, error) {
	if store == nil || runtime == nil || monitored == nil || runtime.scope.Validate() != nil ||
		monitored.Config().ConnConfig.Database != runtime.scope.DatabaseName {
		return nil, ErrScope
	}
	return &EvidenceSource{store, runtime, monitored}, nil
}

var _ probes.HistoricalSource = (*EvidenceSource)(nil)

func (s *EvidenceSource) ReadHistory(ctx context.Context, id probes.ID, args probes.Args) probes.Result {
	return s.readAt(ctx, id, args, time.Now().UTC())
}

func (s *EvidenceSource) readAt(ctx context.Context, id probes.ID, args probes.Args, now time.Time) probes.Result {
	res := probes.Result{ProbeID: id, Version: "v1_history", ObservedAt: now}
	fail := func(reason string) probes.Result {
		res.Rows = nil
		res.Status, res.Reason = probes.StatusUnsupported, reason
		return res
	}
	if err := probes.CheckHistoricalArgs(id, args); err != nil {
		return fail("invalid_args")
	}
	if denied := probes.CheckReadPolicy(ctx, nil, id, args); denied != nil {
		return *denied
	}
	if s == nil || s.runtime == nil || s.store == nil || s.monitored == nil {
		return fail("history_unavailable")
	}
	p := probes.ContextReadPolicy(ctx)
	if p != nil && (p.Database != s.runtime.scope.DatabaseName || p.Binding != probes.PoolBinding(s.monitored)) {
		return probes.PolicyDenied(id, "historical database differs from trusted policy")
	}
	w := args.Window
	if w == 0 {
		w = probes.DefaultWindow
	}
	retained := now.Add(-s.runtime.options.Retention)
	res.History = &probes.HistoricalWindow{ScopeHash: s.runtime.scope.Hash(), From: now.Add(-w), To: now, RetainedFrom: retained}
	from := res.History.From
	if from.Before(retained) {
		from = retained
	}
	streams := map[probes.ID][]Stream{
		probes.LakebasePlans: {Plans}, probes.LakebaseLogs: {Logs}, probes.LakebaseSessions: {ASH},
		probes.LakebaseChanges: {DDL}, probes.LakebaseDatabase: {DatabaseCounters, DatabaseGauges},
	}[id]
	// A single fixed budget covers acquisition, identity, up to two source
	// streams and normalization. Individual backend statements remain bounded
	// separately: a remote store needs more than one statement's round trip.
	// WithTimeout preserves an earlier caller deadline.
	ctx, cancel := context.WithTimeout(ctx, evidenceReadTimeout)
	defer cancel()
	// The pool binding/name cannot detect DROP DATABASE followed by a recreate.
	// Revalidate the physical identity before reading any retained scope data.
	database, oid, err := evidenceDatabaseIdentity(ctx, s.monitored)
	if err != nil {
		res.Status, res.Reason = probes.StatusError, "history_identity_unavailable"
		return res
	}
	if database != s.runtime.scope.DatabaseName || oid != s.runtime.scope.DatabaseOID {
		return fail("history_database_identity_mismatch")
	}
	for _, stream := range streams {
		if !slices.Contains(s.runtime.options.Streams, stream) {
			return fail("history_stream_not_configured")
		}
		events, cursor, err := s.store.evidenceEvents(ctx, s.runtime.scope, s.runtime.binding, stream, Window{from, now}, args.QueryID, evidenceRows+1, now)
		if err != nil {
			if errors.Is(err, ErrPolicy) {
				return fail("history_policy_revoked")
			}
			res.Status, res.Reason = probes.StatusError, "history_read_failed"
			res.Rows = nil
			return res
		}
		if cursor.LastPoll.After(res.History.LastPoll) {
			res.History.LastPoll = cursor.LastPoll
		}
		if res.History.QueryThrough.IsZero() || cursor.ForwardThrough.Before(res.History.QueryThrough) {
			res.History.QueryThrough = cursor.ForwardThrough
		}
		res.History.GapCount += cursor.GapCount
		if len(events) > evidenceRows {
			res.Truncated = true
			events = events[:evidenceRows]
		}
		for _, e := range events {
			if ctx.Err() != nil {
				res.Status, res.Reason, res.Rows = probes.StatusError, "history_read_budget_exhausted", nil
				return res
			}
			row, err := historicalRow(e, args.QueryID)
			switch {
			case errors.Is(err, ErrMasked):
				res.History.UnavailableRows++
			case errors.Is(err, ErrSelfStatement):
				res.History.SelfExcludedRows++
			case err != nil:
				res.History.RejectedRows++
			case row != nil:
				res.Rows = append(res.Rows, row)
			}
		}
	}
	if ctx.Err() != nil {
		res.Status, res.Reason, res.Rows = probes.StatusError, "history_read_budget_exhausted", nil
		return res
	}
	sort.Slice(res.Rows, func(i, j int) bool {
		a, b := res.Rows[i]["occurred_at"].(time.Time), res.Rows[j]["occurred_at"].(time.Time)
		if a.Equal(b) {
			return res.Rows[i]["event_id"].(string) < res.Rows[j]["event_id"].(string)
		}
		return a.After(b)
	})
	if len(res.Rows) > evidenceRows {
		res.Truncated = true
		res.Rows = res.Rows[:evidenceRows]
	}
	if len(res.Rows) == 0 {
		return fail("historical_no_observations_completeness_unknown")
	}
	// Keep all retained results bounded even if a future normalizer adds fields.
	raw, err := json.Marshal(res)
	if err != nil || len(raw) > probes.MaxBytes {
		return fail("historical_result_budget")
	}
	if ctx.Err() != nil {
		res.Status, res.Reason, res.Rows = probes.StatusError, "history_read_budget_exhausted", nil
		return res
	}
	res.Status, res.Reason = probes.StatusOK, "historical_partial_completeness_unknown"
	return res
}

// Acquisition belongs to the whole read budget, not the identity statement's
// budget. Release this transaction before acquiring a store connection so a
// shared single-connection pool cannot deadlock.
func evidenceDatabaseIdentity(ctx context.Context, pool *pgxpool.Pool) (string, int64, error) {
	tx, err := beginEvidenceRead(ctx, pool)
	if err != nil {
		return "", 0, err
	}
	defer rollbackEvidenceRead(tx)
	qctx, cancel := context.WithTimeout(ctx, probes.MaxStatementTimeout)
	defer cancel()
	var database string
	var oid int64
	err = tx.QueryRow(qctx, `SELECT current_database(),oid::bigint FROM pg_database WHERE datname=current_database()`).Scan(&database, &oid)
	return database, oid, err
}

func beginEvidenceRead(ctx context.Context, pool *pgxpool.Pool) (pgx.Tx, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	qctx, cancel := context.WithTimeout(ctx, probes.MaxStatementTimeout)
	defer cancel()
	// Transaction-local settings cannot leak to the next pool borrower.
	_, err = tx.Exec(qctx, `SELECT pg_catalog.set_config('statement_timeout',$1,true),
		pg_catalog.set_config('lock_timeout',$1,true)`, probes.MaxStatementTimeout.String())
	if err != nil {
		rollbackEvidenceRead(tx)
		return nil, err
	}
	return tx, nil
}

// A canceled read still needs to release its transaction and policy lock.
// Cleanup has its own fixed bound and never inherits an exhausted read context.
func rollbackEvidenceRead(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), evidenceRollbackTimeout)
	defer cancel()
	_ = tx.Rollback(ctx)
}

func historicalRow(e Event, query int64) (probes.Row, error) {
	row := probes.Row{"event_id": e.ID, "source": string(e.Stream), "scope_hash": e.ScopeHash,
		"compute_id": e.ComputeID, "occurred_at": e.OccurredAt, "received_at": e.ReceivedAt,
		"historical_only": true, "complete": false, "exact_multiplicity": false}
	switch e.Stream {
	case Plans:
		if !e.PayloadAllowed {
			if selfmonitor.IsApplicationName(e.Fields["application_name"]) {
				return nil, ErrSelfStatement
			}
			qid, err := queryID(e.Fields["queryid"])
			if err != nil {
				return nil, err
			}
			if query != 0 && query != qid {
				return nil, nil
			}
			cost, err := number(e.Fields["total_cost"])
			if err != nil {
				return nil, err
			}
			duration, err := number(e.Fields["duration_ms"])
			if err != nil {
				return nil, err
			}
			row["queryid"], row["total_cost"], row["duration_ms"], row["plan_payload_available"] = qid, cost, duration, false
			row["duration_source"] = "lakebase_plan_history.duration_ms"
			return row, nil
		}
		p, err := NormalizePlan(e)
		if err != nil {
			return nil, err
		}
		if query != 0 && p.QueryID != query {
			return nil, nil
		}
		hash, err := planhash.Compute(p.JSON)
		if err != nil {
			return nil, ErrMalformed
		}
		row["queryid"], row["plan_hash"], row["total_cost"], row["duration_ms"] = p.QueryID, hash, p.TotalCost, p.ExecutionMS
		row["analyze_enabled"], row["buffers_enabled"], row["timing_enabled"], row["duration_source"] = p.AnalyzeEnabled, p.BuffersEnabled, p.TimingEnabled, p.DurationSource
	case Logs:
		entry, err := historicalLog(e)
		if err != nil {
			return nil, err
		}
		row["sqlstate"], row["severity"], row["log_payload_available"] = entry.SQLState, entry.ErrorLevel, e.PayloadAllowed && !missing(e.Fields["message"])
		// One classifier per event prevents its live dedup window from
		// merging historical occurrences. Durable event IDs already dedup.
		c := logwatch.NewClassifier(logwatch.ClassifierConfig{SlowQueryEnabled: true}, func(string, string, ...any) {})
		if signal := c.Classify(entry); signal != nil {
			row["rca_signal"], row["rca_severity"], row["signal_at"] = signal.ID, signal.Severity, signal.FiredAt
		}
		row["session_identity_known"] = false // PID alone is reusable.
	case ASH:
		if selfmonitor.IsApplicationName(e.Fields["application_name"]) || selfmonitor.IsQueryText(e.Fields["query_text"]) {
			return nil, ErrSelfStatement
		}
		s, err := NormalizeASH(e)
		if err != nil {
			return nil, err
		}
		if query != 0 && s.QueryID != query {
			return nil, nil
		}
		row["queryid"], row["pid"], row["blocking_pid"], row["wait_type"], row["wait"] = s.QueryID, s.PID, s.BlockingPID, s.WaitType, s.Wait
		row["query_age_ms"], row["transaction_age_ms"], row["session_identity_known"] = s.QueryAgeMS, s.TransactionAgeMS, false
	case DDL:
		if selfmonitor.IsApplicationName(e.Fields["application_name"]) || selfmonitor.IsQueryText(e.Fields["query_text"]) {
			return nil, ErrSelfStatement
		}
		d, err := NormalizeDDL(e)
		if err != nil {
			return nil, err
		}
		d.Command, d.ObjectType = strings.ToUpper(d.Command), strings.ToUpper(d.ObjectType)
		if !safeDDLCommand(d.Command) || d.ObjectType != "" && !slices.Contains(ddlObjectTypes, d.ObjectType) {
			return nil, ErrMalformed
		}
		row["kind"], row["command"], row["object_type"] = "ddl", d.Command, d.ObjectType
		row["summary"], row["signature"] = "Lakebase historical DDL: "+d.Command, "provider_observed"
	case DatabaseCounters, DatabaseGauges:
		m, err := NormalizeMetrics(e)
		if err != nil {
			return nil, err
		}
		row["interval_from"], row["interval_to"], row["interval_counters"] = m.From, m.To, m.Counters
		for k, v := range m.Values {
			row[k] = v
		}
		row["slo_usable"], row["slo_unknown"] = false, "provider_capture_completeness_and_error_denominator_alignment_unknown"
	default:
		return nil, ErrSchema
	}
	return row, nil
}

var (
	logState       = regexp.MustCompile(`^[0-9A-Z]{5}$`)
	ddlObjectTypes = strings.Split("ACCESS METHOD|AGGREGATE|CAST|COLLATION|CONSTRAINT|CONVERSION|DATABASE|DEFAULT PRIVILEGES|DOMAIN|EVENT TRIGGER|EXTENSION|FOREIGN DATA WRAPPER|FOREIGN TABLE|FUNCTION|INDEX|LANGUAGE|LARGE OBJECT|MATERIALIZED VIEW|OPERATOR|OPERATOR CLASS|OPERATOR FAMILY|POLICY|PROCEDURE|PUBLICATION|ROLE|RULE|SCHEMA|SEQUENCE|SERVER|STATISTICS|SUBSCRIPTION|TABLE|TABLE AS|TABLE COLUMN|TABLE CONSTRAINT|TABLESPACE|TEXT SEARCH CONFIGURATION|TEXT SEARCH DICTIONARY|TEXT SEARCH PARSER|TEXT SEARCH TEMPLATE|TRANSFORM|TRIGGER|TYPE|USER|USER MAPPING|VIEW", "|")
)

func safeDDLCommand(command string) bool {
	if slices.Contains([]string{"COMMENT", "GRANT", "GRANT ROLE", "REVOKE", "REVOKE ROLE", "TRUNCATE", "TRUNCATE TABLE", "REINDEX", "CLUSTER", "REFRESH MATERIALIZED VIEW", "SECURITY LABEL", "DROP OWNED", "REASSIGN OWNED"}, command) {
		return true
	}
	for _, prefix := range []string{"CREATE ", "ALTER ", "DROP "} {
		if strings.HasPrefix(command, prefix) && slices.Contains(ddlObjectTypes, strings.TrimPrefix(command, prefix)) {
			return true
		}
	}
	return false
}

// A masked message does not invalidate a provider's typed SQLSTATE/severity.
// Classification uses only those fields; absent typed fields remain unknown.
// The ordinary log collector keeps its existing full-payload contract.
func historicalLog(e Event) (logwatch.LogEntry, error) {
	entry, err := NormalizeLog(e)
	f := e.Fields
	severityOK := slices.Contains([]string{"DEBUG", "DEBUG1", "DEBUG2", "DEBUG3", "DEBUG4", "DEBUG5", "INFO", "NOTICE", "LOG", "WARNING", "ERROR", "FATAL", "PANIC"}, f["severity"])
	if err == nil && (!severityOK || f["sqlstate"] != "" && !logState.MatchString(f["sqlstate"])) {
		return logwatch.LogEntry{}, ErrMalformed
	}
	if !errors.Is(err, ErrMasked) {
		return entry, err
	}
	state, severity := f["sqlstate"], f["severity"]
	if missing(state) {
		state = ""
	}
	if missing(severity) {
		severity = ""
	}
	if state != "" && !logState.MatchString(state) || severity != "" && !severityOK || state == "" && severity == "" {
		return logwatch.LogEntry{}, ErrMasked
	}
	return logwatch.LogEntry{Timestamp: e.OccurredAt, Database: f["datname"], SQLState: state, ErrorLevel: severity}, nil
}

// evidenceEvents rechecks durable registered-owner policy under a shared lock.
// There is no legacy/unregistered bypass. Read expiry is enforced directly,
// independently of retention cleanup and asynchronous delivery health.
func (s *Store) evidenceEvents(ctx context.Context, scope Scope, binding string, stream Stream, w Window, query int64, limit int, now time.Time) ([]Event, Cursor, error) {
	var cursor Cursor
	if scope.Validate() != nil || !strings.HasPrefix(binding, scope.Hash()+":") || w.Validate() != nil {
		return nil, cursor, ErrScope
	}
	var queryIDs []string
	if query != 0 {
		if stream != Plans && stream != ASH {
			return nil, cursor, ErrScope
		}
		// Provider query IDs may use signed PostgreSQL bigint text or the
		// equivalent unsigned uint64 text. Match both before LIMIT so newer
		// unrelated observations cannot crowd out a requested query.
		queryIDs = []string{strconv.FormatInt(query, 10), strconv.FormatUint(uint64(query), 10)}
	}
	tx, err := beginEvidenceRead(ctx, s.pool)
	if err != nil {
		return nil, cursor, err
	}
	defer rollbackEvidenceRead(tx)
	var allowed []string
	var payload bool
	err = tx.QueryRow(ctx, `SELECT p.allowed_scopes,p.allow_payload FROM sage.lakebaseobs_owner_policy p
	JOIN sage.lakebaseobs_owners o ON o.deployment_id=p.deployment_id AND o.database_id=p.database_id
	WHERE o.deployment_id=$1::uuid AND o.database_id=$2::uuid AND o.scope_hash=$3 FOR SHARE OF p`, scope.DeploymentID, scope.DatabaseID, scope.Hash()).Scan(&allowed, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, cursor, ErrPolicy
	}
	if err != nil {
		return nil, cursor, err
	}
	if !slices.Contains(allowed, scope.Hash()) {
		return nil, cursor, ErrPolicy
	}
	rows, err := tx.Query(ctx, `SELECT event FROM sage.lakebaseobs_inbox WHERE binding=$1 AND stream=$2
	AND occurred_at >= $3 AND occurred_at < $4 AND event IS NOT NULL AND payload_until>$5
	AND ($7::text[] IS NULL OR event->'fields'->>'queryid'=ANY($7::text[]))
	ORDER BY occurred_at DESC,event_id LIMIT $6`, binding, string(stream), w.From, w.To, now, limit, queryIDs)
	if err != nil {
		return nil, cursor, err
	}
	var out []Event
	for rows.Next() {
		var raw []byte
		var e Event
		if err = rows.Scan(&raw); err != nil {
			break
		}
		if json.Unmarshal(raw, &e) != nil {
			err = ErrSchema
			break
		}
		if e.ScopeHash != scope.Hash() || e.Stream != stream {
			err = ErrScope
			break
		}
		if e.PayloadAllowed && !payload {
			err = ErrPolicy
			break
		}
		out = append(out, e)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, cursor, err
	}
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT state FROM sage.lakebaseobs_cursors WHERE binding=$1 AND stream=$2`, binding, string(stream)).Scan(&raw)
	if err == nil && json.Unmarshal(raw, &cursor) != nil {
		return nil, cursor, ErrSchema
	}
	// A retained event can exist before its first cursor in controlled imports.
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, cursor, err
	}
	return out, cursor, nil
}
