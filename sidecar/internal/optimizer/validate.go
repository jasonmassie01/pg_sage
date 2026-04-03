package optimizer

import (
	"context"
	"math"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
)

// Validator runs the P0 bullet-proofing checks on each recommendation.
type Validator struct {
	pool  *pgxpool.Pool
	cfg   *config.OptimizerConfig
	logFn func(string, string, ...any)
}

// NewValidator creates a Validator.
func NewValidator(
	pool *pgxpool.Pool,
	cfg *config.OptimizerConfig,
	logFn func(string, string, ...any),
) *Validator {
	return &Validator{pool: pool, cfg: cfg, logFn: logFn}
}

// Validate runs all checks in order. Returns (accepted, reason).
func (v *Validator) Validate(
	ctx context.Context,
	rec Recommendation,
	tc TableContext,
) (bool, string) {
	if ok, reason := v.checkConcurrently(rec); !ok {
		return false, reason
	}
	if ok, reason := v.checkColumnExistence(rec, tc); !ok {
		return false, reason
	}
	if ok, reason := v.checkDuplicate(rec, tc); !ok {
		return false, reason
	}
	if ok, reason := v.checkWriteImpact(rec, tc); !ok {
		return false, reason
	}
	if ok, reason := v.checkMaxIndexes(tc); !ok {
		return false, reason
	}
	if ok, reason := v.checkExtensionRequired(ctx, rec); !ok {
		return false, reason
	}
	if ok, reason := v.checkBRINCorrelation(rec, tc); !ok {
		return false, reason
	}
	if ok, reason := v.checkExpressionVolatility(ctx, rec); !ok {
		return false, reason
	}
	return true, ""
}

func (v *Validator) checkConcurrently(rec Recommendation) (bool, string) {
	upper := strings.ToUpper(rec.DDL)
	if !strings.Contains(upper, "CONCURRENTLY") {
		return false, "DDL missing CONCURRENTLY keyword"
	}
	return true, ""
}

func (v *Validator) checkColumnExistence(
	rec Recommendation,
	tc TableContext,
) (bool, string) {
	indexCols := extractColumnsFromDDL(rec.DDL)
	if len(indexCols) == 0 {
		return true, ""
	}

	existing := make(map[string]bool)
	for _, c := range tc.Columns {
		existing[strings.ToLower(c.Name)] = true
	}

	for _, col := range indexCols {
		if !existing[strings.ToLower(col)] {
			return false, "column " + col + " does not exist"
		}
	}
	return true, ""
}

func (v *Validator) checkDuplicate(
	rec Recommendation,
	tc TableContext,
) (bool, string) {
	newCols := extractColumnsFromDDL(rec.DDL)
	if len(newCols) == 0 {
		return true, ""
	}

	newKey := normalizeColumnSet(newCols)
	for _, idx := range tc.Indexes {
		existingCols := extractColumnsFromDDL(idx.Definition)
		if normalizeColumnSet(existingCols) == newKey {
			return false, "duplicate of existing index " + idx.Name
		}
	}
	return true, ""
}

func (v *Validator) checkWriteImpact(
	rec Recommendation,
	tc TableContext,
) (bool, string) {
	threshold := v.cfg.WriteImpactThreshPct
	if threshold <= 0 {
		threshold = 15
	}
	if tc.WriteRate > float64(v.cfg.WriteHeavyRatioPct) &&
		rec.EstimatedImprovementPct < threshold {
		return false, "write-heavy table with low estimated improvement"
	}
	return true, ""
}

func (v *Validator) checkMaxIndexes(tc TableContext) (bool, string) {
	max := v.cfg.MaxIndexesPerTable
	if max <= 0 {
		max = 10
	}
	if tc.IndexCount >= max {
		return false, "table already has maximum indexes"
	}
	return true, ""
}

// extensionInstalled checks if a PostgreSQL extension is installed.
func (v *Validator) extensionInstalled(
	ctx context.Context,
	extName string,
) bool {
	if v.pool == nil {
		return true // can't check, assume installed
	}
	var exists bool
	err := v.pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname = $1)",
		extName,
	).Scan(&exists)
	return err == nil && exists
}

// checkExtensionRequired validates that GIN+pg_trgm and GiST+PostGIS
// recommendations have the required extension installed.
func (v *Validator) checkExtensionRequired(
	ctx context.Context,
	rec Recommendation,
) (bool, string) {
	lower := strings.ToLower(rec.DDL)
	if rec.IndexType == "gin" &&
		(strings.Contains(lower, "gin_trgm_ops") || strings.Contains(lower, "trgm")) {
		if !v.extensionInstalled(ctx, "pg_trgm") {
			return false, "requires pg_trgm extension which is not installed"
		}
	}
	if rec.IndexType == "gist" &&
		(strings.Contains(lower, "geometry") || strings.Contains(lower, "geography") ||
			strings.Contains(lower, "st_")) {
		if !v.extensionInstalled(ctx, "postgis") {
			return false, "requires postgis extension which is not installed"
		}
	}
	return true, ""
}

// checkBRINCorrelation rejects BRIN index recommendations when the target
// column has physical correlation below 0.8 (BRIN is useless without it).
func (v *Validator) checkBRINCorrelation(
	rec Recommendation,
	tc TableContext,
) (bool, string) {
	if rec.IndexType != "brin" {
		return true, ""
	}
	cols := extractColumnsFromDDL(rec.DDL)
	if len(cols) == 0 {
		return true, ""
	}
	col := strings.ToLower(cols[0])
	for _, cs := range tc.ColStats {
		if strings.ToLower(cs.Column) == col {
			if math.Abs(cs.Correlation) < 0.8 {
				return false, "BRIN on " + col +
					" rejected: physical correlation too low"
			}
			return true, ""
		}
	}
	return true, ""
}

var funcNameRe = regexp.MustCompile(`\b([a-z_][a-z0-9_]*)\s*\(`)

// checkExpressionVolatility rejects expression index recommendations
// that use non-IMMUTABLE functions (STABLE or VOLATILE break expression indexes).
func (v *Validator) checkExpressionVolatility(
	ctx context.Context,
	rec Recommendation,
) (bool, string) {
	paren := strings.Index(rec.DDL, "(")
	if paren < 0 {
		return true, ""
	}
	inner := rec.DDL[paren+1:]
	nestedParen := strings.Index(inner, "(")
	if nestedParen < 0 {
		return true, "" // no expression, just columns
	}
	matches := funcNameRe.FindStringSubmatch(strings.ToLower(inner))
	if len(matches) < 2 {
		return true, ""
	}
	fnName := matches[1]
	if v.pool == nil {
		return true, ""
	}
	var volatility string
	err := v.pool.QueryRow(ctx,
		"SELECT provolatile::text FROM pg_proc WHERE proname = $1 LIMIT 1",
		fnName,
	).Scan(&volatility)
	if err != nil {
		return true, "" // can't check, allow
	}
	if volatility != "i" {
		return false, "function " + fnName + " is not IMMUTABLE"
	}
	return true, ""
}

// extractColumnsFromDDL parses column names from CREATE INDEX or index def.
func extractColumnsFromDDL(ddl string) []string {
	start := strings.Index(ddl, "(")
	if start < 0 {
		return nil
	}
	end := strings.Index(ddl[start+1:], ")")
	if end < 0 {
		return nil
	}
	colsPart := ddl[start+1 : start+1+end]

	upperDDL := strings.ToUpper(ddl)
	includeIdx := strings.Index(upperDDL, "INCLUDE")
	if includeIdx > start+1 && includeIdx < start+1+end {
		colsPart = ddl[start+1 : includeIdx]
	}

	var cols []string
	for _, part := range strings.Split(colsPart, ",") {
		col := strings.TrimSpace(part)
		col = strings.Trim(col, "\"")
		col = stripSortDirection(col)
		if col != "" {
			cols = append(cols, col)
		}
	}
	return cols
}

func stripSortDirection(col string) string {
	upper := strings.ToUpper(col)
	for _, suffix := range []string{
		" NULLS LAST", " NULLS FIRST", " DESC", " ASC",
	} {
		if strings.HasSuffix(upper, suffix) {
			col = col[:len(col)-len(suffix)]
			upper = strings.ToUpper(col)
		}
	}
	return strings.TrimSpace(col)
}

func normalizeColumnSet(cols []string) string {
	lower := make([]string, len(cols))
	for i, c := range cols {
		lower[i] = strings.ToLower(strings.TrimSpace(c))
	}
	return strings.Join(lower, ",")
}
