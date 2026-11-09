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
	cfg   *config.OptimizerConfig
	logFn func(string, string, ...any)
	// catalog runs the bounded volatility and extension lookups (nil: no
	// database).
	catalog catalogread.Beginner
	// timeouts bound each catalog lookup (the optimizer's catalog bounds).
	timeouts catalogread.Timeouts
	// extensions whose member functions an expression index may call.
	extensions []string
	// extensionSource, when set, answers which extensions are installed
	// instead of the catalog (tests).
	extensionSource func(context.Context, []string) (map[string]bool, error)
}

// NewValidator creates a Validator.
func NewValidator(
	pool *pgxpool.Pool,
	cfg *config.OptimizerConfig,
	logFn func(string, string, ...any),
) *Validator {
	v := &Validator{cfg: cfg, logFn: logFn,
		timeouts: catalogread.Default(), extensions: expressionIndexExtensions}
	if pool != nil {
		v.catalog = pool
	}
	return v
}

// Validate runs all checks in order. Returns (accepted, reason). It is
// ValidateAndQualify without the qualified DDL. The write-impact gate is
// not among the checks: it needs the what-if's measurement, so Admit
// applies it after HypoPG (checkWriteImpact).
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
		func() (bool, string) { return v.checkIncludeWidth(rec) },
		func() (bool, string) { return v.checkDuplicate(rec, tc) },
		func() (bool, string) { return v.checkMaxIndexes(rec, tc) },
		func() (bool, string) { return v.checkExtensionRequired(ctx, rec, tc) },
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
// column (PostgreSQL validates the rest of the expression itself). Without
// this cycle's column list (ColumnsKnown false) it checks nothing: a
// missing list is not evidence that a column does not exist.
func (v *Validator) checkColumnExistence(
	rec Recommendation,
	tc TableContext,
) (bool, string) {
	if !tc.ColumnsKnown {
		return true, ""
	}
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

// defaultMaxIncludeColumns is the INCLUDE cap when max_include_columns is
// unset or not positive.
const defaultMaxIncludeColumns = 3

// checkIncludeWidth rejects an index whose INCLUDE list is wider than
// llm.optimizer.max_include_columns: every INCLUDE column widens each leaf
// entry, so a covering index can cost more than the scans it saves. The
// reason names the cap so the tuning agent can re-propose a narrower index.
func (v *Validator) checkIncludeWidth(rec Recommendation) (bool, string) {
	_, include, ok := indexColumns(rec.DDL)
	if !ok {
		return true, "" // unparseable DDL is rejected by canonicalization
	}
	limit := v.cfg.MaxIncludeColumns
	if limit <= 0 {
		limit = defaultMaxIncludeColumns
	}
	if len(include) > limit {
		return false, fmt.Sprintf("INCLUDE has %d columns, more than "+
			"max_include_columns (%d); propose at most %d INCLUDE columns",
			len(include), limit, limit)
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

// writeHeavy reports whether index-maintaining writes exceed
// write_heavy_ratio_pct of the table's activity.
func (v *Validator) writeHeavy(tc TableContext) bool {
	return tc.WriteRate > float64(v.cfg.WriteHeavyRatioPct)
}

// writeImpactThreshold is write_impact_threshold_pct, 15 when unset.
func (v *Validator) writeImpactThreshold() float64 {
	if v.cfg.WriteImpactThreshPct <= 0 {
		return 15
	}
	return v.cfg.WriteImpactThreshPct
}

// checkWriteImpact rejects an index on a write-heavy table whose measured
// improvement is below write_impact_threshold_pct: every write maintains
// it. Admit calls it only for a complete (verified) measurement.
func (v *Validator) checkWriteImpact(
	rec Recommendation,
	tc TableContext,
) (bool, string) {
	threshold := v.writeImpactThreshold()
	if v.writeHeavy(tc) && rec.EstimatedImprovementPct < threshold {
		return false, fmt.Sprintf("write-heavy table (%.1f%% index-maintaining writes): "+
			"measured improvement %.1f%% is below the %.1f%% write-impact minimum",
			tc.WriteRate, rec.EstimatedImprovementPct, threshold)
	}
	return true, ""
}

// checkMaxIndexes rejects a new index on a table that already has
// max_indexes_per_table valid indexes. A replacement does not add one: the
// valid index it removes is not counted.
func (v *Validator) checkMaxIndexes(rec Recommendation, tc TableContext) (bool, string) {
	limit := v.cfg.MaxIndexesPerTable
	if limit <= 0 {
		limit = 10
	}
	count := tc.IndexCount
	if replacesValidIndex(rec.Replaces, tc.Indexes) {
		count--
	}
	if count >= limit {
		return false, "table already has maximum indexes"
	}
	return true, ""
}

// replacesValidIndex reports whether ref names one of the table's valid
// indexes; when both OIDs are known they must match (a recreated index
// under the same name is another object).
func replacesValidIndex(ref *IndexRef, indexes []IndexInfo) bool {
	if ref == nil || ref.Name == "" {
		return false
	}
	for _, idx := range indexes {
		if idx.Name == ref.Name && idx.IsValid &&
			(ref.OID == 0 || idx.OID == 0 || idx.OID == ref.OID) {
			return true
		}
	}
	return false
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
