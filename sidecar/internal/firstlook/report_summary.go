package firstlook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ReportSummary is what the first-run checklist needs of a report: how
// many items it found, how its catalog checks ended and the capabilities,
// without the items and checks themselves.
type ReportSummary struct {
	ID         int64
	Database   string
	FinishedAt time.Time
	DurationMS int64
	Summary    string
	Items      int
	// Counts sorts the items into findings, setup, posture and other notes.
	Counts ItemCounts
	// Checks counts the catalog checks (agent posture checks are counted
	// in the report's own section).
	Checks       CheckCounts
	Capabilities []Capability
}

// SummaryOf is the summary of a decoded report.
func SummaryOf(r Report) ReportSummary {
	return ReportSummary{ID: r.ID, Database: r.Database, FinishedAt: r.FinishedAt,
		DurationMS: r.DurationMS, Summary: r.Summary, Items: len(r.Items),
		Counts: CountItems(r.Items), Checks: CountChecks(r.Checks, ""),
		Capabilities: r.Capabilities}
}

// latestSummarySQL reads the newest report of $1 with its catalog checks
// ($2 is the degraded status) and its items counted as CountItems counts
// them ($3 the setup rules, $4 and $5 the warning and critical severities).
const latestSummarySQL = `SELECT f.id, f.database_name, f.finished_at,
	f.duration_ms, f.summary, jsonb_array_length(f.items), f.capabilities,
	c.total, c.degraded, c.truncated,
	i.findings, i.critical, i.setup, i.posture, i.notes
	FROM (SELECT * FROM sage.first_look WHERE database_name = $1
		ORDER BY id DESC LIMIT 1) f
	CROSS JOIN LATERAL (SELECT count(*)::int AS total,
		(count(*) FILTER (WHERE e->>'status' = $2))::int AS degraded,
		(count(*) FILTER (WHERE e->'truncated' = 'true'::jsonb))::int AS truncated
		FROM jsonb_array_elements(f.checks) e
		WHERE coalesce(e->>'section', '') = '') c
	CROSS JOIN LATERAL (SELECT
		(count(*) FILTER (WHERE k = 'finding'))::int AS findings,
		(count(*) FILTER (WHERE k = 'finding' AND sev = $5))::int AS critical,
		(count(*) FILTER (WHERE k = 'setup'))::int AS setup,
		(count(*) FILTER (WHERE k = 'posture'))::int AS posture,
		(count(*) FILTER (WHERE k = 'note'))::int AS notes
		FROM (SELECT e->>'severity' AS sev, CASE
			WHEN coalesce(e->>'section', '') <> '' THEN 'posture'
			WHEN coalesce(e->>'rule', '') = ANY($3::text[]) THEN 'setup'
			WHEN e->>'severity' IN ($4, $5) THEN 'finding'
			ELSE 'note' END AS k
			FROM jsonb_array_elements(f.items) e) kinds) i`

// LatestSummary returns the summary of database's newest report, counted
// in SQL so the items and checks are never decoded; found is false when no
// report exists yet. It counts exactly what SummaryOf counts of Latest.
func (s *Store) LatestSummary(ctx context.Context, database string) (
	ReportSummary, bool, error) {
	if err := s.ready(); err != nil {
		return ReportSummary{}, false, err
	}
	var r ReportSummary
	var caps []byte
	n := &r.Counts
	err := s.pool.QueryRow(ctx, latestSummarySQL, database, string(CheckDegraded),
		setupRules, SeverityWarning, SeverityCritical).Scan(&r.ID, &r.Database,
		&r.FinishedAt, &r.DurationMS, &r.Summary, &r.Items, &caps, &r.Checks.Total,
		&r.Checks.Degraded, &r.Checks.Truncated, &n.Findings, &n.Critical, &n.Setup,
		&n.Posture, &n.Notes)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReportSummary{}, false, nil
	}
	if err != nil {
		return ReportSummary{}, false, fmt.Errorf("read first look summary of %q: %w",
			database, err)
	}
	if err := json.Unmarshal(caps, &r.Capabilities); err != nil {
		return ReportSummary{}, false, fmt.Errorf(
			"decode first look capabilities of %q: %w", database, err)
	}
	return r, true, nil
}
