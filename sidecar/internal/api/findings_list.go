package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/fleet"
)

// sevRankSQL ranks severity, most severe highest. The findings list index
// idx_findings_list_severity is built on this exact expression.
const sevRankSQL = "(CASE severity WHEN 'critical' THEN 3 WHEN 'warning' THEN 2 " +
	"WHEN 'info' THEN 1 ELSE 0 END)"

func findingSeverityRank(sev string) int64 {
	switch sev {
	case "critical":
		return 3
	case "warning":
		return 2
	case "info":
		return 1
	}
	return 0
}

// findingSortKeys is each sort's ORDER BY (id breaks the last tie). The
// severity and last_seen sorts, the ones the dashboard uses, are served
// by idx_findings_list_severity and idx_findings_list_last_seen; the
// others sort one status's rows.
func findingSortKeys(sortName string) []sortKey {
	lastSeen := sortKey{"last_seen", keyTime}
	rank := sortKey{sevRankSQL, keyInt}
	switch sortName {
	case "severity":
		return []sortKey{rank, lastSeen}
	case "created_at":
		return []sortKey{{"created_at", keyTime}}
	case "category":
		return []sortKey{{`category COLLATE "C"`, keyText}}
	case "title":
		return []sortKey{{`title COLLATE "C"`, keyText}}
	case "impact", "impact_score":
		return []sortKey{{"COALESCE(impact_score, '-Infinity'::real)", keyFloat}, rank,
			lastSeen}
	default:
		return []sortKey{lastSeen}
	}
}

const findingsColumns = `/* pg_sage */SELECT id, created_at, last_seen,
 occurrence_count, category, severity, object_type,
 object_identifier, title, detail, recommendation,
 recommended_sql, rollback_sql, status, rule_id, impact_score,
 resolved_at, acted_on_at, action_log_id`

// findingsFilterSQL is the list's WHERE clause. A severity filter is also
// stated on the rank so the severity index bounds the scan.
func findingsFilterSQL(f fleet.FindingFilters) (string, []any) {
	where, args := buildFindingsWhere(f)
	if f.Severity != "" {
		args = append(args, findingSeverityRank(f.Severity))
		where += fmt.Sprintf(" AND %s = $%d", sevRankSQL, len(args))
	}
	return where, args
}

// buildFindingsPageSQL selects up to limit findings after cur (all from
// the start without one), ordered by the sort's keys, which it also
// returns as trailing columns for the next cursor and the fleet merge.
func buildFindingsPageSQL(
	f fleet.FindingFilters, cur *listCursor, source string, limit int,
) (string, []any) {
	where, args := findingsFilterSQL(f)
	keys := findingSortKeys(f.Sort)
	desc := f.Order != "asc"
	var bound int64
	if cur != nil {
		bound = keysetBoundID(source, cur.Source, cur.ID, desc)
	}
	order, after, kargs := keysetSQL(keys, desc, cur, bound, len(args)+1)
	args = append(args, kargs...)
	sel := findingsColumns
	for _, k := range keys {
		sel += ", " + k.expr
	}
	args = append(args, limit)
	return sel + " FROM sage.findings" + where + after + order +
		fmt.Sprintf(" LIMIT $%d", len(args)), args
}

// pagedRow is one list row with its position: sort keys, source, id.
type pagedRow struct {
	row    map[string]any
	keys   []any
	source string
	id     int64
}

// pageResult is one source's rows (sorted) and its capped total.
type pageResult struct {
	rows   []pagedRow
	total  int
	capped bool
}

// errBadPage marks a request whose cursor or offset cannot be served.
var errBadPage = errors.New("bad page request")

func checkCursorKeys(cur *listCursor, keys []sortKey) error {
	if cur != nil && len(cur.Keys) != len(keys) {
		return fmt.Errorf("%w: cursor does not match the sort", errBadPage)
	}
	return nil
}

// queryFindingsPage reads one database's next offset+limit+1 rows after
// the cursor, and, when asked, its total capped at maxListTotal.
func queryFindingsPage(
	ctx context.Context, pool *pgxpool.Pool, f fleet.FindingFilters, page listPage,
	source string, withTotal bool,
) (pageResult, error) {
	var res pageResult
	keys := findingSortKeys(f.Sort)
	if err := checkCursorKeys(page.Cursor, keys); err != nil {
		return res, err
	}
	if withTotal {
		where, args := findingsFilterSQL(f)
		total, capped, err := cappedCount(ctx, pool, "sage.findings"+where, args)
		if err != nil {
			return res, fmt.Errorf("count findings: %w", err)
		}
		res.total, res.capped = total, capped
	}
	sql, args := buildFindingsPageSQL(f, page.Cursor, source, page.Offset+page.Limit+1)
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return res, fmt.Errorf("query findings: %w", err)
	}
	defer rows.Close()
	res.rows, err = scanFindingPage(rows, source, keys)
	return res, err
}

// cappedCount counts the rows of "<from> WHERE ..." up to maxListTotal+1:
// the reported total is exact below the cap and capped above it.
func cappedCount(ctx context.Context, pool *pgxpool.Pool, fromWhere string,
	args []any) (int, bool, error) {
	var n int
	err := pool.QueryRow(ctx, fmt.Sprintf(
		"/* pg_sage */ SELECT count(*) FROM (SELECT 1 FROM %s LIMIT %d) capped",
		fromWhere, maxListTotal+1), args...).Scan(&n)
	if err != nil {
		return 0, false, err
	}
	return min(n, maxListTotal), n > maxListTotal, nil
}

func scanFindingPage(rows pgx.Rows, source string, keys []sortKey) ([]pagedRow, error) {
	var out []pagedRow
	for rows.Next() {
		var (
			id                                         int64
			createdAt, lastSeen                        time.Time
			occurrences                                int
			category, severity, title, status          string
			objectType, objectIdent, ruleID            *string
			detail                                     []byte
			recommendation, recommendedSQL, rollbackSQ *string
			impact                                     *float64
			resolvedAt, actedOnAt                      *time.Time
			actionLogID                                *int64
		)
		dest := []any{&id, &createdAt, &lastSeen, &occurrences, &category, &severity,
			&objectType, &objectIdent, &title, &detail, &recommendation, &recommendedSQL,
			&rollbackSQ, &status, &ruleID, &impact, &resolvedAt, &actedOnAt, &actionLogID}
		keyDests := make([]any, len(keys))
		for i, k := range keys {
			keyDests[i] = keyDest(k)
		}
		if err := rows.Scan(append(dest, keyDests...)...); err != nil {
			return nil, fmt.Errorf("scan finding: %w", err)
		}
		row := buildFindingMapWithAction(id, createdAt, lastSeen, occurrences, category,
			severity, objectType, objectIdent, title, detail, recommendation, recommendedSQL,
			rollbackSQ, status, source, ruleID, impact, resolvedAt, actedOnAt, actionLogID)
		out = append(out, pagedRow{row: row, keys: keyValues(keyDests), source: source, id: id})
	}
	return out, rows.Err()
}

func keyValues(dests []any) []any {
	out := make([]any, len(dests))
	for i, d := range dests {
		out[i] = keyValue(d)
	}
	return out
}

// mergePage orders rows from any number of sources by (keys, source, id),
// skips the offset and returns one page and the cursor after it (empty
// when nothing follows).
func mergePage(rows []pagedRow, page listPage, desc bool, sortName, order string,
) ([]map[string]any, string) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if c := compareKeys(a.keys, b.keys); c != 0 {
			return (c < 0) != desc
		}
		if a.source != b.source {
			return a.source < b.source
		}
		return (a.id < b.id) != desc
	})
	start := min(page.Offset, len(rows))
	end := min(start+page.Limit, len(rows))
	out := make([]map[string]any, 0, end-start)
	for _, r := range rows[start:end] {
		out = append(out, r.row)
	}
	if len(rows) <= end || end == start {
		return out, ""
	}
	last := rows[end-1]
	keys := make([]string, len(last.keys))
	for i, k := range last.keys {
		keys[i] = encodeKey(k)
	}
	return out, encodeListCursor(listCursor{Sort: sortName, Order: order, Keys: keys,
		Source: last.source, ID: last.id})
}

// listFindings serves one page of findings from the selected databases.
func listFindings(
	ctx context.Context, sources []namedPool, f fleet.FindingFilters, page listPage,
) (map[string]any, error) {
	var all []pagedRow
	total, capped := 0, false
	for _, s := range sources {
		res, err := queryFindingsPage(ctx, s.pool, f, page, s.name, true)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.name, err)
		}
		all = append(all, res.rows...)
		total += res.total
		capped = capped || res.capped
	}
	findings, next := mergePage(all, page, f.Order != "asc", f.Sort, f.Order)
	total, capped = capTotal(total, capped)
	return map[string]any{"filters": f, "total": total, "total_capped": capped,
		"offset": page.Offset, "limit": page.Limit, "findings": findings,
		"next_cursor": next}, nil
}

// writeListError answers a list request that failed.
func writeListError(w http.ResponseWriter, r *http.Request, what string, err error) {
	if errors.Is(err, errBadPage) {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	internalError(w, r, "query "+what, err)
}

// listSources resolves a list request's databases: the named one (under
// its display name), the only one, or every connected one for "all".
func listSources(mgr *fleet.DatabaseManager, database string) []namedPool {
	if database == "all" && mgr.InstanceCount() > 1 {
		return poolsForDatabaseSelection(mgr, "all")
	}
	pool := mgr.PoolForDatabase(database)
	if pool == nil {
		return nil
	}
	return []namedPool{{name: mgr.ResolveDatabaseName(database), pool: pool}}
}
