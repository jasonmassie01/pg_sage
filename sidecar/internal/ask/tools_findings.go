package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/agentloop"
)

// Findings and the approval queue, read from the monitored database's
// sage schema. Every read is bounded and parameterized.

var (
	findingStatuses = map[string]bool{"open": true, "resolved": true, "suppressed": true,
		"all": true}
	severities = map[string]bool{"info": true, "warning": true, "critical": true}
	wordArg    = regexp.MustCompile(`^[a-z0-9_:.-]{1,100}$`)
)

func (ss *session) recordTools() []agentloop.Tool {
	return []agentloop.Tool{
		tool("list_findings", "List the database's findings (pg_sage's detected problems "+
			"and recommendations), newest first.", `"status":{"type":"string","enum":`+
			`["open","resolved","suppressed","all"]},"severity":{"type":"string","enum":`+
			`["info","warning","critical"]},"category":{"type":"string","maxLength":100},`+
			`"limit":{"type":"integer","minimum":1,"maximum":20}`, nil, ss.listFindings),
		tool("get_finding", "Read one finding: its detail, recommended SQL, rollback and "+
			"whether a fix waits for approval or ran.", `"id":{"type":"integer",`+
			`"minimum":1}`, []string{"id"}, ss.getFinding),
		tool("list_approvals", "List the fixes waiting for a person's approval.",
			`"limit":{"type":"integer","minimum":1,"maximum":20}`, nil, ss.listApprovals),
		tool("list_actions", "List the actions pg_sage executed, newest first, with their "+
			"verification verdict.", `"limit":{"type":"integer","minimum":1,"maximum":20}`,
			nil, ss.listActions),
		tool("get_action", "Read one executed action with its verification outcome: "+
			"predicted versus observed effect.", `"id":{"type":"integer","minimum":1}`,
			[]string{"id"}, ss.getAction),
		tool("list_facts", "List the typed facts about the database (owned by the app's "+
			"migrations, test fixtures, slot consumers, append-only tables, windows).",
			`"status":{"type":"string","enum":["proposed","confirmed","rejected",`+
				`"expired","all"]}`, nil, ss.listFacts),
		tool("list_incidents", "List incidents (detected root causes).",
			`"status":{"type":"string","enum":["active","all"]},"limit":{"type":"integer",`+
				`"minimum":1,"maximum":20}`, nil, ss.listIncidents),
	}
}

type findingsArgs struct {
	Status   string `json:"status"`
	Severity string `json:"severity"`
	Category string `json:"category"`
	Limit    *int   `json:"limit"`
}

func (a findingsArgs) check() (findingsArgs, int, error) {
	if a.Status == "" {
		a.Status = "open"
	}
	limit, err := limitOf(a.Limit, 10, 20)
	switch {
	case err != nil:
		return a, 0, err
	case !findingStatuses[a.Status]:
		return a, 0, invalidArgs("unknown status %q", a.Status)
	case a.Severity != "" && !severities[a.Severity]:
		return a, 0, invalidArgs("unknown severity %q", a.Severity)
	case a.Category != "" && !wordArg.MatchString(a.Category):
		return a, 0, invalidArgs("invalid category")
	}
	return a, limit, nil
}

func (a findingsArgs) evidenceID() string {
	id := "findings:" + a.Status
	if a.Severity != "" || a.Category != "" {
		id += ":" + a.Severity
	}
	if a.Category != "" {
		id += ":" + a.Category
	}
	return id
}

func (ss *session) listFindings(ctx context.Context, raw json.RawMessage) (
	agentloop.Output, error) {
	var a findingsArgs
	if err := decodeArgs(raw, &a); err != nil {
		return agentloop.Output{}, err
	}
	a, limit, err := a.check()
	if err != nil {
		return agentloop.Output{}, err
	}
	rows, err := ss.s.d.Pool.Query(ctx, `/* pg_sage */ SELECT id, severity, category,
		title, COALESCE(object_identifier, ''), occurrence_count, last_seen, status,
		COALESCE(impact_score, 0) FROM sage.findings
		WHERE ($1 = 'all' OR status = $1) AND ($2 = '' OR severity = $2)
		  AND ($3 = '' OR category = $3)
		ORDER BY last_seen DESC, id DESC LIMIT $4`, a.Status, a.Severity, a.Category, limit)
	if err != nil {
		return agentloop.Output{}, fmt.Errorf("read findings: %w", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var id int64
		var sev, cat, title, object, status string
		var count int
		var seen time.Time
		var impact float64
		if err := rows.Scan(&id, &sev, &cat, &title, &object, &count, &seen, &status,
			&impact); err != nil {
			return agentloop.Output{}, fmt.Errorf("read findings: %w", err)
		}
		lines = append(lines, fmt.Sprintf("finding %d [%s/%s/%s] %s | object %s | "+
			"seen %d times, last %s | impact %.2f", id, status, sev, cat, oneLine(title),
			object, count, ts(seen), impact))
	}
	if err := rows.Err(); err != nil {
		return agentloop.Output{}, fmt.Errorf("read findings: %w", err)
	}
	return ss.listOutput(a.evidenceID(), lines, "no findings match (status "+a.Status+")"), nil
}

// listOutput is a citable list, or a citable "empty".
func (ss *session) listOutput(id string, lines []string, empty string) agentloop.Output {
	if len(lines) == 0 {
		return ss.cite(id, "empty", empty)
	}
	return ss.cite(id, "ok", fmt.Sprintf("%d rows:\n%s", len(lines),
		strings.Join(lines, "\n")))
}

type idArgs struct {
	ID int64 `json:"id"`
}

func decodeID(raw json.RawMessage) (int64, error) {
	var a idArgs
	if err := decodeArgs(raw, &a); err != nil {
		return 0, err
	}
	if a.ID < 1 {
		return 0, invalidArgs("id must be a positive integer")
	}
	return a.ID, nil
}

func (ss *session) getFinding(ctx context.Context, raw json.RawMessage) (agentloop.Output,
	error) {
	id, err := decodeID(raw)
	if err != nil {
		return agentloop.Output{}, err
	}
	evID := fmt.Sprintf("finding:%d", id)
	text, err := ss.findingText(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ss.cite(evID, "not_found", fmt.Sprintf("finding %d does not exist", id)), nil
	}
	if err != nil {
		return agentloop.Output{}, fmt.Errorf("read finding %d: %w", id, err)
	}
	return ss.cite(evID, "ok", text), nil
}

func (ss *session) findingText(ctx context.Context, id int64) (string, error) {
	var status, sev, cat, title, objType, object, rec, sql, rollback string
	var count int
	var created, seen time.Time
	var detail []byte
	var actionID *int64
	err := ss.s.d.Pool.QueryRow(ctx, `/* pg_sage */ SELECT status, severity, category,
		title, COALESCE(object_type, ''), COALESCE(object_identifier, ''),
		COALESCE(recommendation, ''), COALESCE(recommended_sql, ''),
		COALESCE(rollback_sql, ''), occurrence_count, created_at, last_seen, detail,
		action_log_id FROM sage.findings WHERE id = $1`, id).Scan(&status, &sev, &cat,
		&title, &objType, &object, &rec, &sql, &rollback, &count, &created, &seen, &detail,
		&actionID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "finding %d status %s, severity %s, category %s\ntitle: %s\n"+
		"object: %s %s\nfirst seen %s, last seen %s, %d occurrences\n", id, status, sev, cat,
		oneLine(title), objType, object, ts(created), ts(seen), count)
	optional(&b, "recommendation", oneLine(rec))
	optional(&b, "recommended SQL", sql)
	optional(&b, "rollback SQL", rollback)
	fmt.Fprintf(&b, "detail: %s\n", sanitizedDetail(detail))
	if actionID != nil {
		fmt.Fprintf(&b, "executed as action %d\n", *actionID)
	}
	pending, err := ss.pendingFor(ctx, id)
	b.WriteString(pending)
	return strings.TrimSpace(b.String()), err
}

func optional(b *strings.Builder, label, value string) {
	if strings.TrimSpace(value) != "" {
		fmt.Fprintf(b, "%s: %s\n", label, value)
	}
}

// pendingFor lists the finding's items waiting for approval.
func (ss *session) pendingFor(ctx context.Context, id int64) (string, error) {
	rows, err := ss.s.d.Pool.Query(ctx, `/* pg_sage */ SELECT id, proposed_at
		FROM sage.action_queue WHERE finding_id = $1 AND status = 'pending'
		ORDER BY id DESC LIMIT 5`, id)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var q int64
		var at time.Time
		if err := rows.Scan(&q, &at); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "pending approval %d (proposed %s)\n", q, ts(at))
	}
	return b.String(), rows.Err()
}

func (ss *session) listApprovals(ctx context.Context, raw json.RawMessage) (
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
	rows, err := ss.s.d.Pool.Query(ctx, `/* pg_sage */ SELECT id, COALESCE(finding_id, 0),
		proposed_sql, COALESCE(action_risk, ''), COALESCE(policy_decision, ''), proposed_at
		FROM sage.action_queue WHERE status = 'pending'
		  AND (expires_at IS NULL OR expires_at > now())
		ORDER BY proposed_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return agentloop.Output{}, fmt.Errorf("read approvals: %w", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var id, finding int64
		var sql, risk, decision string
		var at time.Time
		if err := rows.Scan(&id, &finding, &sql, &risk, &decision, &at); err != nil {
			return agentloop.Output{}, fmt.Errorf("read approvals: %w", err)
		}
		lines = append(lines, fmt.Sprintf("queue %d [pending] finding %d: %s (risk %s, "+
			"policy %s, proposed %s)", id, finding, oneLine(sql), risk, decision, ts(at)))
	}
	if err := rows.Err(); err != nil {
		return agentloop.Output{}, fmt.Errorf("read approvals: %w", err)
	}
	return ss.listOutput("approvals:pending", lines, "nothing waits for approval"), nil
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
