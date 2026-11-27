package selfmonitor

import (
	"encoding/json"
	"maps"
	"regexp"
	"slices"
	"strings"
)

const (
	SchemaName      = "sage"
	ApplicationName = "pg_sage"
)

const QueryTextSQLRegex = `(^|[^[:alnum:]_])("?sage"?)[[:space:]]*\.`

var sageSchemaRef = regexp.MustCompile(`(?i)(^|[^a-z0-9_])("?sage"?)[[:space:]]*\.`)

// SelfFindingCategories are the findings about pg_sage's own cost on a
// database (its database time, budgets and storage footprint). They are
// pg_sage's subject on purpose, keyed by the database name, and may name
// pg_sage, so the self-monitoring filters never drop them.
var SelfFindingCategories = []string{"sage_self_cost", "sage_self_budget", "sage_footprint"}

// ExplanationDetailKeys are finding detail keys whose value is pg_sage's
// own explanation to the operator, such as why a change waits for approval
// (analyzer.DetailApprovalRequired). Such prose may name pg_sage ("pg_sage
// cannot verify it"), which says nothing about the finding's subject, so the
// detail scan of IsFinding skips them.
var ExplanationDetailKeys = []string{"approval_required"}

// FindingFields are the parts of a finding that tell whether it is about
// pg_sage itself.
type FindingFields struct {
	Category         string
	ObjectType       string
	ObjectIdentifier string
	Title            string
	Detail           map[string]any
	RecommendedSQL   string
	RollbackSQL      string
}

// IsQueryText reports a statement that is pg_sage's own: it carries the
// pg_sage tag or reads the sage schema. Reading a statistics view is not
// pg_sage's signature (a Datadog, pganalyze or pgwatch agent reads the
// same views), so an untagged catalog read is another client's.
func IsQueryText(query string) bool {
	query = strings.TrimSpace(query)
	if query == "" {
		return false
	}
	lower := strings.ToLower(query)
	return strings.Contains(lower, ApplicationName) || sageSchemaRef.MatchString(query)
}

// IsObjectIdentifier reports an object in the sage schema: a
// schema-qualified sage. name of any type, or the bare name sage when the
// object is a schema (a database or a user table may be named sage).
func IsObjectIdentifier(objectType, identifier string) bool {
	identifier = strings.TrimSpace(strings.ToLower(identifier))
	if identifier == "" {
		return false
	}
	if identifier == SchemaName {
		return strings.EqualFold(strings.TrimSpace(objectType), "schema")
	}
	return strings.HasPrefix(identifier, SchemaName+".") ||
		strings.HasPrefix(identifier, `"`+SchemaName+`".`)
}

// IsFinding reports a finding about pg_sage itself, which advice and the
// finding views leave out. The self categories are kept whatever they name.
func IsFinding(f FindingFields) bool {
	if slices.Contains(SelfFindingCategories, f.Category) {
		return false
	}
	if IsObjectIdentifier(f.ObjectType, f.ObjectIdentifier) {
		return true
	}
	if strings.Contains(strings.ToLower(f.Title), ApplicationName) {
		return true
	}
	if IsQueryText(f.RecommendedSQL) || IsQueryText(f.RollbackSQL) {
		return true
	}
	return detailHasSelfQuery(f.Detail)
}

// sqlSpaceChars are the ASCII characters strings.TrimSpace removes, as a
// btrim set, so the SQL trims identifiers as IsObjectIdentifier does.
const sqlSpaceChars = `E' \t\n\r\f\v'`

// FindingsSQLExclusionClause is IsFinding as a WHERE fragment over
// sage.findings (AND-ed onto a clause): the rows to show.
func FindingsSQLExclusionClause() string {
	ident := `lower(btrim(coalesce(object_identifier, ''), ` + sqlSpaceChars + `))`
	return ` AND (category IN ('` + strings.Join(SelfFindingCategories, `', '`) +
		`') OR NOT (` +
		`(` + ident + ` = 'sage' AND ` +
		`lower(btrim(coalesce(object_type, ''), ` + sqlSpaceChars + `)) = 'schema') OR ` +
		ident + ` LIKE 'sage.%' OR ` +
		ident + ` LIKE '"sage".%' OR ` +
		`coalesce(title, '') ILIKE '%pg_sage%' OR ` +
		sqlTextExpr("recommended_sql") + ` OR ` +
		sqlTextExpr("rollback_sql") + ` OR ` +
		sqlJSONTextExpr("query") + ` OR ` +
		sqlJSONTextExpr("query_text") + ` OR ` +
		sqlJSONTextExpr("normalized_query") + ` OR ` +
		sqlJSONTextExpr("sample_query") + ` OR ` +
		sqlJSONTextExpr("sql") + ` OR ` +
		sqlJSONTextExpr("statement") +
		`))`
}

func sqlTextExpr(column string) string {
	return `(coalesce(` + column + `, '') ILIKE '%pg_sage%' OR ` +
		`coalesce(` + column + `, '') ~* '` + QueryTextSQLRegex + `')`
}

func sqlJSONTextExpr(key string) string {
	return `(coalesce(detail->>'` + key + `', '') ILIKE '%pg_sage%' OR ` +
		`coalesce(detail->>'` + key + `', '') ~* '` + QueryTextSQLRegex + `')`
}

func detailHasSelfQuery(detail map[string]any) bool {
	if len(detail) == 0 {
		return false
	}
	for _, key := range []string{
		"query", "query_text", "normalized_query",
		"sample_query", "sql", "statement", "recommended_sql",
	} {
		if IsQueryText(stringFromAny(detail[key])) {
			return true
		}
	}
	b, err := json.Marshal(withoutExplanations(detail))
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(b)), ApplicationName)
}

// withoutExplanations is a copy of detail without its
// ExplanationDetailKeys.
func withoutExplanations(detail map[string]any) map[string]any {
	out := maps.Clone(detail)
	for _, key := range ExplanationDetailKeys {
		delete(out, key)
	}
	return out
}

func stringFromAny(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return ""
	}
}
