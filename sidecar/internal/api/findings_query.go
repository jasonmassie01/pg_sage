package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

func queryFindingsStats(
	ctx context.Context, pool *pgxpool.Pool,
	f fleet.FindingFilters,
) ([]map[string]any, int, error) {
	where, args := buildFindingsWhere(f)
	query := `SELECT severity, category, COUNT(*) AS cnt
	 FROM sage.findings` + where +
		` GROUP BY severity, category
		  ORDER BY severity, category`
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf(
			"query findings stats: %w", err)
	}
	defer rows.Close()

	var results []map[string]any
	total := 0
	for rows.Next() {
		var severity, category string
		var cnt int
		if err := rows.Scan(
			&severity, &category, &cnt,
		); err != nil {
			return nil, 0, fmt.Errorf(
				"scan findings stat: %w", err)
		}
		total += cnt
		results = append(results, map[string]any{
			"severity": severity,
			"category": category,
			"count":    cnt,
		})
	}
	if results == nil {
		results = []map[string]any{}
	}
	return results, total, nil
}

func queryFindingsStatsAcrossFleet(
	ctx context.Context, mgr *fleet.DatabaseManager,
	f fleet.FindingFilters,
) ([]map[string]any, int, error) {
	counts := make(map[string]int)
	total := 0
	for _, inst := range sortedFleetInstances(mgr) {
		if inst.Pool == nil {
			continue
		}
		stats, dbTotal, err := queryFindingsStats(
			ctx, inst.Pool, f,
		)
		if err != nil {
			return nil, 0, err
		}
		total += dbTotal
		for _, s := range stats {
			key := fmt.Sprintf("%s\x00%s",
				s["severity"], s["category"])
			if c, ok := s["count"].(int); ok {
				counts[key] += c
			}
		}
	}
	results := make([]map[string]any, 0, len(counts))
	for key, count := range counts {
		parts := strings.SplitN(key, "\x00", 2)
		results = append(results, map[string]any{
			"severity": parts[0],
			"category": parts[1],
			"count":    count,
		})
	}
	sort.Slice(results, func(i, j int) bool {
		si, sj := fmt.Sprint(results[i]["severity"]),
			fmt.Sprint(results[j]["severity"])
		if si != sj {
			return si < sj
		}
		return fmt.Sprint(results[i]["category"]) <
			fmt.Sprint(results[j]["category"])
	})
	if results == nil {
		results = []map[string]any{}
	}
	return results, total, nil
}

func parseFindingFilters(q map[string][]string) fleet.FindingFilters {
	f := fleet.FindingFilters{
		Status:           valOrDefault(q, "status", "open"),
		Severity:         valOrDefault(q, "severity", ""),
		Category:         valOrDefault(q, "category", ""),
		Source:           valOrDefault(q, "source", ""),
		ThematicCategory: valOrDefault(q, "thematic_category", ""),
		Sort:             valOrDefault(q, "sort", "severity"),
		Order:            valOrDefault(q, "order", "desc"),
		Limit:            parseIntDefault(firstVal(q, "limit"), 50),
		Offset:           parseIntDefault(firstVal(q, "offset"), 0),
		From:             parseTimeParam(firstVal(q, "from")),
		To:               parseTimeParam(firstVal(q, "to")),
	}
	if f.Limit > 200 {
		f.Limit = 200
	}
	return f
}

func findingsEmptyResponse(
	database string, filters fleet.FindingFilters,
) map[string]any {
	return map[string]any{
		"database":     database,
		"filters":      filters,
		"total":        0,
		"total_capped": false,
		"offset":       filters.Offset,
		"limit":        filters.Limit,
		"findings":     []any{},
		"next_cursor":  "",
	}
}

func sortedFleetInstances(
	mgr *fleet.DatabaseManager,
) []*fleet.DatabaseInstance {
	instances := mgr.Instances()
	names := make([]string, 0, len(instances))
	for name := range instances {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]*fleet.DatabaseInstance, 0, len(names))
	for _, name := range names {
		out = append(out, instances[name])
	}
	return out
}

func buildFindingsWhere(
	f fleet.FindingFilters,
) (string, []any) {
	where := " WHERE 1=1"
	var args []any
	n := 1
	where += selfmonitor.FindingsSQLExclusionClause()
	if f.Status != "" {
		where += fmt.Sprintf(" AND status = $%d", n)
		args = append(args, f.Status)
		n++
	}
	if f.Severity != "" {
		where += fmt.Sprintf(" AND severity = $%d", n)
		args = append(args, f.Severity)
		n++
	}
	if f.Category != "" {
		where += fmt.Sprintf(" AND category = $%d", n)
		args = append(args, f.Category)
		n++
	}
	// Source filter — subsystem taxonomy mapping (see §16 of
	// reviews/archive/docs-internal/ui-redesign-v2.md). The mapping is an allowlist-to-SQL
	// rewrite. Unknown source values are treated as no-filter so the
	// UI can add new subsystems ahead of backend awareness without
	// dropping the page.
	if clause, a, ok := buildSourceClause(f.Source, n); ok {
		where += clause
		args = append(args, a...)
		n += len(a)
	}
	// ThematicCategory only applies when rows are lint rows, whose
	// detail JSONB carries thematic_category ('indexing',
	// 'performance', etc.). For non-lint queries the filter is
	// effectively a no-op since their detail lacks that key.
	if f.ThematicCategory != "" {
		where += fmt.Sprintf(
			" AND detail->>'thematic_category' = $%d", n)
		args = append(args, f.ThematicCategory)
		n++
	}
	// Overlapping-window time filter (findings live across time):
	// include a finding when it was open at any point in [From, To].
	if !f.From.IsZero() {
		where += fmt.Sprintf(
			" AND (resolved_at IS NULL OR resolved_at >= $%d)", n)
		args = append(args, f.From)
		n++
	}
	if !f.To.IsZero() {
		where += fmt.Sprintf(" AND created_at <= $%d", n)
		args = append(args, f.To)
	}
	return where, args
}

func buildFindingMapWithAction(
	id int64, createdAt, lastSeen time.Time,
	occurrenceCount int, category, severity string,
	objectType, objectIdent *string, title string,
	detail []byte, recommendation, recommendedSQL, rollbackSQL *string,
	status, database string, ruleID *string,
	impactScore *float64, resolvedAt, actedOnAt *time.Time,
	actionLogID *int64,
) map[string]any {
	// detail is jsonb in the DB, but a corrupt row or pre-jsonb
	// legacy bytes can land here as invalid JSON. Surface the raw
	// string on decode failure so operators still see the data,
	// and log once so the malformed row is traceable.
	var detailParsed any
	if len(detail) > 0 {
		if err := json.Unmarshal(detail, &detailParsed); err != nil {
			slog.Warn(
				"findings: detail column is not valid JSON",
				"finding_id", id,
				"err", err,
			)
			detailParsed = string(detail)
		}
	}
	normalizeFindingQueryID(detailParsed, derefStr(objectType), derefStr(objectIdent))
	return withFindingRisk(map[string]any{
		"id":                strconv.FormatInt(id, 10),
		"created_at":        createdAt,
		"last_seen":         lastSeen,
		"occurrence_count":  occurrenceCount,
		"category":          category,
		"severity":          severity,
		"object_type":       derefStr(objectType),
		"object_identifier": derefStr(objectIdent),
		"title":             title,
		"detail":            detailParsed,
		"recommendation":    derefStr(recommendation),
		"recommended_sql":   derefStr(recommendedSQL),
		"rollback_sql":      derefStr(rollbackSQL),
		"status":            status,
		"database_name":     database,
		"rule_id":           derefStr(ruleID),
		"impact_score":      impactScore,
		"resolved_at":       resolvedAt,
		"acted_on_at":       actedOnAt,
		"action_log_id":     actionLogID,
		"subsystem":         subsystemFromCategory(category),
	}, derefStr(recommendedSQL), detailParsed)
}

func queryFindingByID(
	ctx context.Context, pool *pgxpool.Pool, id string,
) (map[string]any, error) {
	var (
		fID              int64
		createdAt        time.Time
		lastSeen         time.Time
		occurrenceCount  int
		category         string
		severity         string
		objectType       *string
		objectIdent      *string
		title            string
		detail           []byte
		recommendation   *string
		recommendedSQL   *string
		rollbackSQL      *string
		estimatedCostUSD *float64
		status           string
		suppressedUntil  *time.Time
		resolvedAt       *time.Time
		actedOnAt        *time.Time
		actionLogID      *int64
	)
	err := pool.QueryRow(ctx, findingDetailSQL, id).Scan(
		&fID, &createdAt, &lastSeen, &occurrenceCount,
		&category, &severity, &objectType, &objectIdent,
		&title, &detail, &recommendation, &recommendedSQL,
		&rollbackSQL, &estimatedCostUSD, &status,
		&suppressedUntil, &resolvedAt, &actedOnAt,
		&actionLogID,
	)
	if err != nil {
		return nil, err
	}
	// See getFindings — bad JSON returns the raw string rather than
	// masquerading as a missing field, with a log entry for audit.
	var detailParsed any
	if len(detail) > 0 {
		if err := json.Unmarshal(detail, &detailParsed); err != nil {
			slog.Warn(
				"finding_detail: detail column is not valid JSON",
				"finding_id", fID,
				"err", err,
			)
			detailParsed = string(detail)
		}
	}
	normalizeFindingQueryID(detailParsed, derefStr(objectType), derefStr(objectIdent))
	return withFindingRisk(map[string]any{
		"id":                 strconv.FormatInt(fID, 10),
		"created_at":         createdAt,
		"last_seen":          lastSeen,
		"occurrence_count":   occurrenceCount,
		"category":           category,
		"severity":           severity,
		"object_type":        derefStr(objectType),
		"object_identifier":  derefStr(objectIdent),
		"title":              title,
		"detail":             detailParsed,
		"recommendation":     derefStr(recommendation),
		"recommended_sql":    derefStr(recommendedSQL),
		"rollback_sql":       derefStr(rollbackSQL),
		"estimated_cost_usd": estimatedCostUSD,
		"status":             status,
		"suppressed_until":   suppressedUntil,
		"resolved_at":        resolvedAt,
		"acted_on_at":        actedOnAt,
		"action_log_id":      actionLogID,
	}, derefStr(recommendedSQL), detailParsed), nil
}

const findingDetailSQL = `/* pg_sage */SELECT id, created_at, last_seen,
 occurrence_count, category, severity, object_type,
 object_identifier, title, detail, recommendation,
 recommended_sql, rollback_sql, estimated_cost_usd,
 status, suppressed_until, resolved_at, acted_on_at,
 action_log_id
 FROM sage.findings WHERE id = $1`

func updateFindingStatus(
	ctx context.Context, pool *pgxpool.Pool,
	id, fromStatus, toStatus string,
) error {
	tag, err := pool.Exec(ctx,
		`/* pg_sage */ UPDATE sage.findings SET status = $1
		 WHERE id = $2 AND status = $3`,
		toStatus, id, fromStatus,
	)
	if err != nil {
		// Unique constraint on (category, object_identifier)
		// for open findings — an open finding already exists.
		if strings.Contains(err.Error(), "idx_findings_dedup") {
			// Delete the stale suppressed finding instead of
			// unsuppressing it since there's already an active
			// open finding for the same issue.
			_, delErr := pool.Exec(ctx,
				`/* pg_sage */ DELETE FROM sage.findings
				 WHERE id = $1 AND status = 'suppressed'`, id)
			if delErr != nil {
				return fmt.Errorf(
					"conflict cleanup failed: %w", delErr)
			}
			return nil
		}
		return fmt.Errorf("update finding status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no matching finding")
	}
	return nil
}
