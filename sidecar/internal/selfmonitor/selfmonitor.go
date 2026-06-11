package selfmonitor

import (
	"encoding/json"
	"regexp"
	"strings"
)

const (
	SchemaName      = "sage"
	ApplicationName = "pg_sage"
)

const QueryTextSQLRegex = `(^|[^[:alnum:]_])("?sage"?)[[:space:]]*\.`

var sageSchemaRef = regexp.MustCompile(`(?i)(^|[^a-z0-9_])("?sage"?)[[:space:]]*\.`)

// collectorCatalogRef matches pg_sage's own monitoring reads of the
// statistics catalogs. Historical findings captured before queries were
// tagged with /* pg_sage */ carry no marker and never reference the sage
// schema, so this signature is needed to recognize them as self-queries.
var collectorCatalogRef = regexp.MustCompile(
	`(?is)\bfrom[[:space:]]+(pg_stat_statements|pg_stat_user_tables|` +
		`pg_stat_user_indexes|pg_stat_activity|pg_stat_database|` +
		`pg_stat_replication|pg_stat_bgwriter|pg_stat_checkpointer)\b`)

type FindingFields struct {
	ObjectIdentifier string
	Title            string
	Detail           map[string]any
	RecommendedSQL   string
	RollbackSQL      string
}

func IsQueryText(query string) bool {
	query = strings.TrimSpace(query)
	if query == "" {
		return false
	}
	lower := strings.ToLower(query)
	return strings.Contains(lower, ApplicationName) ||
		sageSchemaRef.MatchString(query) ||
		collectorCatalogRef.MatchString(query)
}

func IsObjectIdentifier(identifier string) bool {
	identifier = strings.TrimSpace(strings.ToLower(identifier))
	if identifier == "" {
		return false
	}
	return identifier == SchemaName ||
		strings.HasPrefix(identifier, SchemaName+".") ||
		strings.HasPrefix(identifier, `"`+SchemaName+`".`)
}

func IsFinding(f FindingFields) bool {
	if IsObjectIdentifier(f.ObjectIdentifier) {
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

func FindingsSQLExclusionClause() string {
	return ` AND NOT (` +
		`lower(coalesce(object_identifier, '')) = 'sage' OR ` +
		`lower(coalesce(object_identifier, '')) LIKE 'sage.%' OR ` +
		`coalesce(title, '') ILIKE '%pg_sage%' OR ` +
		sqlTextExpr("recommended_sql") + ` OR ` +
		sqlTextExpr("rollback_sql") + ` OR ` +
		sqlJSONTextExpr("query") + ` OR ` +
		sqlJSONTextExpr("query_text") + ` OR ` +
		sqlJSONTextExpr("normalized_query") + ` OR ` +
		sqlJSONTextExpr("sample_query") + ` OR ` +
		sqlJSONTextExpr("sql") + ` OR ` +
		sqlJSONTextExpr("statement") +
		`)`
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
	b, err := json.Marshal(detail)
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(b)), ApplicationName)
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
