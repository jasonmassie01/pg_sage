package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/llm"
)

// Executed actions with their verification outcomes (predicted versus
// observed), facts and incidents.

func (ss *session) listActions(ctx context.Context, raw json.RawMessage) (
	agentloop.Output, error) {
	var a struct {
		Limit *int `json:"limit"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return agentloop.Output{}, err
	}
	limit, err := limitOf(a.Limit, 10, 20)
	if err != nil {
		return agentloop.Output{}, err
	}
	rows, err := ss.s.d.Pool.Query(ctx, `/* pg_sage */ SELECT l.id, l.action_type,
		COALESCE(l.finding_id, 0), l.executed_at, l.outcome,
		COALESCE(o.verdict, ''), COALESCE(o.tolerance, '')
		FROM sage.action_log l LEFT JOIN sage.action_outcome o ON o.action_log_id = l.id
		ORDER BY l.executed_at DESC, l.id DESC LIMIT $1`, limit)
	if err != nil {
		return agentloop.Output{}, fmt.Errorf("read actions: %w", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var id, finding int64
		var typ, outcome, verdict, tolerance string
		var at time.Time
		if err := rows.Scan(&id, &typ, &finding, &at, &outcome, &verdict,
			&tolerance); err != nil {
			return agentloop.Output{}, fmt.Errorf("read actions: %w", err)
		}
		verification := "no verification outcome"
		if verdict != "" {
			verification = "verification " + verdict + "/" + tolerance
		}
		lines = append(lines, fmt.Sprintf("action %d [%s] finding %d executed %s, "+
			"outcome %s; %s", id, typ, finding, ts(at), outcome, verification))
	}
	if err := rows.Err(); err != nil {
		return agentloop.Output{}, fmt.Errorf("read actions: %w", err)
	}
	return ss.listOutput("actions:recent", lines, "pg_sage has executed no actions"), nil
}

func (ss *session) getAction(ctx context.Context, raw json.RawMessage) (agentloop.Output,
	error) {
	id, err := decodeID(raw)
	if err != nil {
		return agentloop.Output{}, err
	}
	evID := fmt.Sprintf("action:%d", id)
	text, err := ss.actionText(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ss.cite(evID, "not_found", fmt.Sprintf("action %d does not exist", id)), nil
	}
	if err != nil {
		return agentloop.Output{}, fmt.Errorf("read action %d: %w", id, err)
	}
	return ss.cite(evID, "ok", text), nil
}

func (ss *session) actionText(ctx context.Context, id int64) (string, error) {
	var typ, sql, rollback, outcome, approvedBy, verdict, tolerance, method, class string
	var reason string
	var at time.Time
	var predicted, observed []byte
	err := ss.s.d.Pool.QueryRow(ctx, `/* pg_sage */ SELECT l.action_type, l.sql_executed,
		COALESCE(l.rollback_sql, ''), l.outcome, COALESCE('user ' || l.approved_by, ''),
		l.executed_at,
		COALESCE(o.verdict, ''), COALESCE(o.tolerance, ''),
		COALESCE(o.prediction_method, ''), COALESCE(o.action_class, ''),
		COALESCE(o.predicted, '{}'::jsonb), COALESCE(o.observed, '{}'::jsonb),
		COALESCE(o.reason, '')
		FROM sage.action_log l LEFT JOIN sage.action_outcome o ON o.action_log_id = l.id
		WHERE l.id = $1`, id).Scan(&typ, &sql, &rollback, &outcome, &approvedBy, &at,
		&verdict, &tolerance, &method, &class, &predicted, &observed, &reason)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "action %d type %s, outcome %s, executed %s\n", id, typ, outcome, ts(at))
	if approvedBy != "" {
		b.WriteString("approved by " + approvedBy + "\n")
	}
	optional(&b, "SQL", sql)
	optional(&b, "rollback SQL", rollback)
	if verdict == "" {
		b.WriteString("no verification outcome recorded\n")
		return strings.TrimSpace(b.String()), nil
	}
	fmt.Fprintf(&b, "verification outcome: verdict %s, tolerance %s, method %s, class %s\n"+
		"predicted: %s\nobserved: %s\n", verdict, tolerance, method, class, predicted, observed)
	optional(&b, "reason", reason)
	return strings.TrimSpace(b.String()), nil
}

func (ss *session) listFacts(ctx context.Context, raw json.RawMessage) (agentloop.Output,
	error) {
	var a struct {
		Status string `json:"status"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return agentloop.Output{}, err
	}
	if a.Status == "" {
		a.Status = "all"
	}
	filter := facts.Filter{Limit: 30}
	switch facts.Status(a.Status) {
	case facts.StatusProposed, facts.StatusConfirmed, facts.StatusRejected,
		facts.StatusExpired:
		filter.Status = []facts.Status{facts.Status(a.Status)}
	default:
		if a.Status != "all" {
			return agentloop.Output{}, invalidArgs("unknown status %q", a.Status)
		}
	}
	got, err := facts.NewStore(ss.s.d.Pool).List(ctx, filter)
	if err != nil {
		return agentloop.Output{}, fmt.Errorf("read facts: %w", err)
	}
	lines := make([]string, 0, len(got))
	for _, f := range got {
		lines = append(lines, fmt.Sprintf("fact %d [%s] %s %s %s: %s (%s)", f.ID, f.Status,
			f.Type, f.Kind, f.Subject, f.Describe(), f.Provenance()))
	}
	return ss.listOutput("facts:"+a.Status, lines, "no "+a.Status+" facts"), nil
}

func (ss *session) listIncidents(ctx context.Context, raw json.RawMessage) (
	agentloop.Output, error) {
	var a struct {
		Status string `json:"status"`
		Limit  *int   `json:"limit"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return agentloop.Output{}, err
	}
	if a.Status == "" {
		a.Status = "active"
	}
	limit, err := limitOf(a.Limit, 10, 20)
	if err != nil {
		return agentloop.Output{}, err
	}
	if a.Status != "active" && a.Status != "all" {
		return agentloop.Output{}, invalidArgs("unknown status %q", a.Status)
	}
	lines, err := ss.incidentLines(ctx, a.Status == "active", limit)
	if err != nil {
		return agentloop.Output{}, err
	}
	return ss.listOutput("incidents:"+a.Status, lines, "no "+a.Status+" incidents"), nil
}

func (ss *session) incidentLines(ctx context.Context, active bool, limit int) ([]string,
	error) {
	rows, err := ss.s.d.Pool.Query(ctx, `/* pg_sage */ SELECT id::text, severity,
		root_cause, array_to_string(affected_objects, ', '), source, detected_at,
		last_detected_at, occurrence_count, resolved_at FROM sage.incidents
		WHERE NOT $1 OR resolved_at IS NULL
		ORDER BY last_detected_at DESC LIMIT $2`, active, limit)
	if err != nil {
		return nil, fmt.Errorf("read incidents: %w", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var id, sev, cause, objects, source string
		var detected, last time.Time
		var count int
		var resolved *time.Time
		if err := rows.Scan(&id, &sev, &cause, &objects, &source, &detected, &last, &count,
			&resolved); err != nil {
			return nil, fmt.Errorf("read incidents: %w", err)
		}
		state := "active"
		if resolved != nil {
			state = "resolved " + ts(*resolved)
		}
		lines = append(lines, fmt.Sprintf("incident %s [%s, %s] %s | objects %s | source "+
			"%s | detected %s, last %s, %d occurrences", id, sev, state, oneLine(cause),
			objects, source, ts(detected), ts(last), count))
	}
	return lines, rows.Err()
}

// sanitizedDetail is a finding's detail JSON with SQL-bearing values
// (query text, plans) redacted the way every prompt site does.
func sanitizedDetail(raw []byte) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return "{}"
	}
	out, err := json.Marshal(sanitizeValue("", v))
	if err != nil {
		return "{}"
	}
	return clip(string(out), 2000)
}

func sanitizeValue(key string, v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			x[k] = sanitizeValue(k, child)
		}
		return x
	case []any:
		for i, child := range x {
			x[i] = sanitizeValue(key, child)
		}
		return x
	case string:
		lower := strings.ToLower(key)
		if strings.Contains(lower, "query") || strings.Contains(lower, "sql") ||
			strings.Contains(lower, "plan") || strings.Contains(lower, "statement") {
			return llm.SanitizeForLLM(x)
		}
	}
	return v
}
