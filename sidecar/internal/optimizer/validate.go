package optimizer

import (
	"context"
	"math"
	"regexp"
	"sort"
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

// checkColumnExistence requires every plain key and INCLUDE column to
// exist, and every expression key to reference at least one existing
// column (PostgreSQL validates the rest of the expression itself).
func (v *Validator) checkColumnExistence(
	rec Recommendation,
	tc TableContext,
) (bool, string) {
	keys, include, ok := indexColumns(rec.DDL)
	if !ok {
		return true, "" // unparseable DDL is rejected by canonicalization
	}
	existing := make(map[string]bool, len(tc.Columns))
	for _, c := range tc.Columns {
		existing[strings.ToLower(c.Name)] = true
	}
	for _, key := range append(keys, include...) {
		if key.column != "" && !existing[strings.ToLower(key.column)] {
			return false, "column " + key.column + " does not exist"
		}
		if key.expr != "" && !anyExists(key.refs, existing) {
			return false, "expression " + key.expr + " references no existing column"
		}
	}
	return true, ""
}

func anyExists(refs []string, existing map[string]bool) bool {
	for _, r := range refs {
		if existing[strings.ToLower(r)] {
			return true
		}
	}
	return false
}

// checkDuplicate rejects a recommendation whose shape — method, key
// columns/expressions, INCLUDE set and predicate — equals a valid
// existing index, or that a valid existing index covers (coveredBy).
// Invalid indexes (a failed CONCURRENTLY build) are not
// duplicates of anything.
func (v *Validator) checkDuplicate(
	rec Recommendation,
	tc TableContext,
) (bool, string) {
	want, ok := shapeOf(rec.DDL)
	if !ok {
		return true, ""
	}
	for _, idx := range tc.Indexes {
		if !idx.IsValid {
			continue
		}
		if have, ok := shapeOf(idx.Definition); ok && have == want {
			return false, "duplicate of existing index " + idx.Name
		}
		if CoveredBy(rec.DDL, idx.Definition) {
			return false, "covered by existing index " + idx.Name
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

// checkExpressionVolatility rejects an index whose key expressions or
// predicate call a function with no IMMUTABLE overload (PostgreSQL
// refuses STABLE/VOLATILE functions in index expressions). Unknown names
// (keywords such as IN, or a lookup error) are left to PostgreSQL.
func (v *Validator) checkExpressionVolatility(
	ctx context.Context,
	rec Recommendation,
) (bool, string) {
	spec, err := ParseIndexDDL(rec.DDL)
	if err != nil || v.pool == nil {
		return true, ""
	}
	seen := map[string]bool{}
	for _, m := range funcNameRe.FindAllStringSubmatch(spec.Keys+" "+spec.Where, -1) {
		fn := m[1]
		if seen[fn] {
			continue
		}
		seen[fn] = true
		var immutable *bool
		err := v.pool.QueryRow(ctx,
			"SELECT bool_or(provolatile = 'i') FROM pg_proc WHERE proname = $1",
			fn).Scan(&immutable)
		if err != nil {
			v.logFn("optimizer", "volatility lookup for %s failed: %v", fn, err)
			continue
		}
		if immutable != nil && !*immutable {
			return false, "function " + fn + " is not IMMUTABLE"
		}
	}
	return true, ""
}

// extractColumnsFromDDL returns the plain key columns of a CREATE INDEX
// statement or index definition, in order; expression keys are skipped.
// It returns nil when the DDL cannot be parsed.
func extractColumnsFromDDL(ddl string) []string {
	keys, err := parseIndexKeys(ddl)
	if err != nil {
		return nil
	}
	var cols []string
	for _, k := range keys {
		if k.column != "" {
			cols = append(cols, k.column)
		}
	}
	return cols
}

// indexColumns parses the key and INCLUDE lists of an index DDL.
func indexColumns(ddl string) (keys, include []indexKey, ok bool) {
	spec, err := ParseIndexDDL(ddl)
	if err != nil {
		return nil, nil, false
	}
	keys, err = splitKeyList(spec.Keys)
	if err != nil {
		return nil, nil, false
	}
	if spec.Include != "" {
		if include, err = splitKeyList(spec.Include); err != nil {
			return nil, nil, false
		}
	}
	return keys, include, true
}

// indexShape is the comparable form of an index definition.
type indexShape struct {
	method, keys, include, where string
}

// shapeOf parses an index DDL or pg_get_indexdef definition into its
// comparable shape: casts and grouping parentheses are dropped so the
// catalog form "lower((email)::text)" equals "lower(email)" and
// "WHERE (status = 'open'::text)" equals "WHERE status = 'open'".
func shapeOf(ddl string) (indexShape, bool) {
	spec, err := ParseIndexDDL(ddl)
	if err != nil {
		return indexShape{}, false
	}
	keys, include, ok := indexColumns(ddl)
	if !ok {
		return indexShape{}, false
	}
	inc := keyShapes(include)
	sort.Strings(inc)
	return indexShape{method: spec.Method, keys: strings.Join(keyShapes(keys), ","),
		include: strings.Join(inc, ","), where: canonicalShape(spec.Where)}, true
}

func keyShapes(keys []indexKey) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k.column
		if k.expr != "" {
			out[i] = canonicalShape(k.expr)
		}
	}
	return out
}

var castPattern = regexp.MustCompile(
	`::\s*(?:"[^"]*"|[a-z_][a-z0-9_]*)(?:\s+varying)?(?:\(\s*\d+(?:\s*,\s*\d+)?\s*\))?(?:\[\])*`)

// canonicalShape drops casts and parentheses from a normalized fragment
// and collapses whitespace.
func canonicalShape(frag string) string {
	s := castPattern.ReplaceAllString(frag, "")
	s = strings.NewReplacer("(", " ", ")", " ").Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	return strings.NewReplacer(" ,", ",", ", ", ",").Replace(s)
}
