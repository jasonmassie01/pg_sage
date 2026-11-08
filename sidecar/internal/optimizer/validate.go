package optimizer

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/config"
)

// Validator runs the P0 bullet-proofing checks on each recommendation.
type Validator struct {
	pool  *pgxpool.Pool
	cfg   *config.OptimizerConfig
	logFn func(string, string, ...any)
	// catalog runs the bounded volatility lookups (nil: no database).
	catalog catalogread.Beginner
	// timeouts bound each catalog lookup (the optimizer's catalog bounds).
	timeouts catalogread.Timeouts
	// extensions whose member functions an expression index may call.
	extensions []string
}

// NewValidator creates a Validator.
func NewValidator(
	pool *pgxpool.Pool,
	cfg *config.OptimizerConfig,
	logFn func(string, string, ...any),
) *Validator {
	v := &Validator{pool: pool, cfg: cfg, logFn: logFn,
		timeouts: catalogread.Default(), extensions: expressionIndexExtensions}
	if pool != nil {
		v.catalog = pool
	}
	return v
}

// Validate runs all checks in order. Returns (accepted, reason). It is
// ValidateAndQualify without the qualified DDL.
func (v *Validator) Validate(
	ctx context.Context,
	rec Recommendation,
	tc TableContext,
) (bool, string) {
	_, ok, reason := v.ValidateAndQualify(ctx, rec, tc)
	return ok, reason
}

// ValidateAndQualify runs all checks in order and returns rec with every
// bare function its index expressions call named by the schema it binds to
// (pg_catalog.lower(email)), so the DDL builds the same index on any
// search_path (security-01). Returns (rec, accepted, reason).
func (v *Validator) ValidateAndQualify(
	ctx context.Context,
	rec Recommendation,
	tc TableContext,
) (Recommendation, bool, string) {
	for _, check := range []func() (bool, string){
		func() (bool, string) { return v.checkConcurrently(rec) },
		func() (bool, string) { return v.checkColumnExistence(rec, tc) },
		func() (bool, string) { return v.checkDuplicate(rec, tc) },
		func() (bool, string) { return v.checkWriteImpact(rec, tc) },
		func() (bool, string) { return v.checkMaxIndexes(tc) },
		func() (bool, string) { return v.checkExtensionRequired(ctx, rec) },
		func() (bool, string) { return v.checkBRINCorrelation(rec, tc) },
	} {
		if ok, reason := check(); !ok {
			return rec, false, reason
		}
	}
	ddl, reason := v.resolveIndexFunctions(ctx, rec)
	if reason != "" {
		return rec, false, reason
	}
	rec.DDL = ddl
	return rec, true, ""
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
// columns/expressions with their opclass, collation and order, INCLUDE
// set and predicate — equals a valid existing index, or that a valid
// existing index covers (coveredBy). The table's column defaults
// normalize both sides first: pg_get_indexdef omits a default opclass and
// a collation equal to the column's, while the model may spell them out.
// Invalid indexes (a failed CONCURRENTLY build) are not
// duplicates of anything.
func (v *Validator) checkDuplicate(
	rec Recommendation,
	tc TableContext,
) (bool, string) {
	want, ok := shapeOf(rec.DDL, tc.KeyDefaults)
	if !ok {
		return true, ""
	}
	for _, idx := range tc.Indexes {
		if !idx.IsValid {
			continue
		}
		if have, ok := shapeOf(idx.Definition, tc.KeyDefaults); ok && have == want {
			return false, "duplicate of existing index " + idx.Name
		}
		if covers(idx.Definition, rec.DDL, false, tc.KeyDefaults) {
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

// resolveIndexFunctions binds every function an index's key expressions
// and predicate call (bindFunction) and returns the DDL with each bare call
// named by its bound schema. A call bound outside pg_catalog and the
// allowlisted extensions (a function planted in public, or none at all),
// one that is not IMMUTABLE in every overload of its bound schema
// (PostgreSQL refuses STABLE and VOLATILE functions in index expressions),
// and a call the name match cannot see (a quoted name, a comment, a
// character Go folds unlike PostgreSQL) are refused with a reason; a failed
// lookup refuses the index. ParseIndexDDL folds unquoted ASCII names to
// lower case, as PostgreSQL does. The reason is "" when the index is
// admitted.
func (v *Validator) resolveIndexFunctions(
	ctx context.Context, rec Recommendation,
) (string, string) {
	spec, err := ParseIndexDDL(rec.DDL)
	if err != nil || v.catalog == nil {
		return rec.DDL, ""
	}
	frag := spec.Keys + " " + spec.Where
	if unverifiableCallRe.MatchString(frag) ||
		(strings.Contains(frag, "(") && foldsUnlikePostgres(rec.DDL)) {
		return "", "function_schema_not_allowed: a quoted function or schema " +
			"name, a comment, or a character PostgreSQL reads differently, in an " +
			"index expression cannot be verified"
	}
	bound := map[funcCall]string{}
	for _, c := range functionCalls(frag) {
		found, err := v.functionSchemas(ctx, c)
		if err != nil {
			return "", fmt.Sprintf("volatility lookup for %s failed, index refused: %v", c, err)
		}
		schema, reason := bindFunction(c, found)
		if reason != "" {
			return "", reason
		}
		bound[c] = schema
	}
	return qualifyBoundCalls(rec.DDL, bound)
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
// "WHERE (status = 'open'::text)" equals "WHERE status = 'open'". Keys
// keep their opclass, collation and (btree) order; defaults drop the
// spellings that equal a column's defaults.
func shapeOf(ddl string, defaults map[string]KeyDefault) (indexShape, bool) {
	spec, err := ParseIndexDDL(ddl)
	if err != nil {
		return indexShape{}, false
	}
	keys, include, ok := indexColumns(ddl)
	if !ok {
		return indexShape{}, false
	}
	inc := keyBases(include)
	sort.Strings(inc)
	return indexShape{method: spec.Method,
		keys:    strings.Join(shapeKeys(keys, spec.Method, defaults), ","),
		include: strings.Join(inc, ","), where: canonicalShape(spec.Where)}, true
}

var castPattern = regexp.MustCompile(
	`::\s*(?:"[^"]*"|[a-z_][a-z0-9_]*)(?:\s+varying)?(?:\(\s*\d+(?:\s*,\s*\d+)?\s*\))?(?:\[\])*`)

// canonicalShape drops casts, parentheses and pg_catalog qualifiers from a
// normalized fragment and collapses whitespace.
func canonicalShape(frag string) string {
	s := castPattern.ReplaceAllString(dropCatalogQualifiers(frag), "")
	s = strings.NewReplacer("(", " ", ")", " ").Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	return strings.NewReplacer(" ,", ",", ", ", ",").Replace(s)
}
