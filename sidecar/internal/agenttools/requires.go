package agenttools

// RequiredExtensions names, per coding-agent tool (by its MCP name), the
// extension without which the tool cannot work at all on a database. MCP
// does not list such a tool for a database known to lack the extension.
// top_queries reads only pg_stat_statements; whatif_index measures the
// pg_stat_statements queries its query_ids name (without HypoPG it says so
// instead of failing, but its query ids still come only from there). The
// other tools work without any extension; requires_db_test.go proves it.
var RequiredExtensions = map[string]string{
	"top_queries":  "pg_stat_statements",
	"whatif_index": "pg_stat_statements",
}
