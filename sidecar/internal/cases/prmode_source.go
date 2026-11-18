package cases

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/verify"
)

// prFinding is the sage.findings row a PR candidate is built from.
type prFinding struct {
	ID                                   int64
	Title, Recommendation, SQL, Rollback string
	Detail                               map[string]any
}

// prCandidatesSQL reads findings whose PR-mode PR is still being followed
// (open, merged or deployed, or regressed without a revert yet) first, then
// open findings that carry a change, newest first.
const prCandidatesSQL = `/* pg_sage */ SELECT f.id, f.title, COALESCE(f.recommendation, ''),
    COALESCE(f.recommended_sql, ''), COALESCE(f.rollback_sql, ''), f.detail
FROM sage.findings f
LEFT JOIN sage.source_fix s ON s.finding_id = f.id
WHERE (f.status = 'open' AND s.id IS NULL AND (COALESCE(f.recommended_sql, '') <> ''
        OR f.detail ? 'source_fix' OR f.detail ? 'suggested_rewrite'))
   OR (s.origin = 'pg_sage' AND COALESCE(s.pr_state, '') NOT IN ('closed', 'revert_opened')
        AND (s.verdict IS NULL OR s.verdict = 'regressed'))
ORDER BY (s.id IS NOT NULL) DESC, f.last_seen DESC, f.id DESC
LIMIT $1`

// prScanFactor bounds the rows one load reads per candidate wanted: most
// open findings carry a change the executor runs itself, which is no
// candidate, so a limit on rows alone would starve older migrations.
const prScanFactor = 25

// LoadPRCandidates reads up to limit PR candidates from sage.findings:
// changes the executor runs under no contract (classify), fact-bound
// source fixes, and query rewrites. It reads at most limit*prScanFactor
// findings.
func LoadPRCandidates(ctx context.Context, pool *pgxpool.Pool, classify SQLClassifier,
	limit int) ([]PRCandidate, error) {
	if limit <= 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, prStoreTimeout)
	defer cancel()
	rows, err := pool.Query(ctx, prCandidatesSQL, limit*prScanFactor)
	if err != nil {
		return nil, fmt.Errorf("read PR candidates: %w", err)
	}
	defer rows.Close()
	var out []PRCandidate
	for rows.Next() {
		var f prFinding
		var detail []byte
		if err := rows.Scan(&f.ID, &f.Title, &f.Recommendation, &f.SQL, &f.Rollback,
			&detail); err != nil {
			return nil, fmt.Errorf("scan PR candidate: %w", err)
		}
		dec := json.NewDecoder(bytes.NewReader(detail))
		dec.UseNumber() // query ids exceed float64 precision
		if err := dec.Decode(&f.Detail); err != nil {
			return nil, fmt.Errorf("decode finding %d detail: %w", f.ID, err)
		}
		if c, ok := candidateFromFinding(f, classify); ok {
			if out = append(out, c); len(out) == limit {
				break
			}
		}
	}
	return out, rows.Err()
}

// candidateFromFinding is f's PR candidate, if it has one.
func candidateFromFinding(f prFinding, classify SQLClassifier) (PRCandidate, bool) {
	c := PRCandidate{FindingID: f.ID, Title: f.Title, Why: f.Recommendation,
		EvidenceIDs: []string{fmt.Sprintf("finding:%d", f.ID)}}
	if ladder, err := verify.DecodeLadderValue(f.Detail["ladder"]); err == nil {
		c.Ladder = ladder
	}
	if fix, ok := f.Detail["source_fix"].(map[string]any); ok {
		if up := strings.TrimSpace(detailString(fix, "migration", "")); up != "" {
			c.Kind, c.MigrationSQL = PRKindMigration, up
			c.RollbackSQL = detailString(fix, "down", "")
			c.RiskTier = RiskTierForSQL(up, classify)
			return c, true
		}
	}
	if sql := strings.TrimSpace(f.SQL); sql != "" {
		if kind, tier := classifySQL(sql, classify); kind == ddlPreflightType &&
			appMigration(sql) {
			c.Kind, c.MigrationSQL, c.RollbackSQL, c.RiskTier = PRKindMigration, sql,
				strings.TrimSpace(f.Rollback), tier
			return c, true
		}
	}
	rewrite := strings.TrimSpace(detailString(f.Detail, "suggested_rewrite", ""))
	original := detailString(f.Detail, "original_query", detailString(f.Detail, "query", ""))
	if rewrite == "" || strings.TrimSpace(original) == "" {
		return PRCandidate{}, false
	}
	c.Kind, c.RiskTier, c.OriginalSQL, c.RewrittenSQL = PRKindRewrite, "moderate",
		original, rewrite
	c.QueryID = prQueryID(f.Detail)
	c.RewriteChecked = detailString(f.Detail, "rewrite_equivalence", "") == "passed"
	return c, true
}

// prQueryID is the finding's queryid, exactly (a json.Number or string).
func prQueryID(detail map[string]any) int64 {
	switch v := detail["queryid"].(type) {
	case json.Number:
		n, _ := strconv.ParseInt(v.String(), 10, 64)
		return n
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

// appMigration reports DDL that belongs in an application's migrations: a
// schema object's CREATE, ALTER, DROP or COMMENT. Maintenance (VACUUM,
// REINDEX), data removal (TRUNCATE), privileges (GRANT, REVOKE, default
// privileges) and cluster-wide objects (ALTER SYSTEM, roles, databases,
// tablespaces, subscriptions, event triggers) stay advice for a person.
func appMigration(sql string) bool {
	words := strings.Fields(strings.ToUpper(sql))
	if len(words) < 2 {
		return false
	}
	object := words[1]
	switch words[0] {
	case "CREATE", "ALTER", "DROP":
	case "COMMENT":
		if len(words) < 3 {
			return false
		}
		object = words[2] // COMMENT ON <object>
	default:
		return false
	}
	switch object {
	case "SYSTEM", "ROLE", "USER", "GROUP", "DATABASE", "TABLESPACE", "SUBSCRIPTION",
		"DEFAULT", "EVENT":
		return false
	}
	return true
}
