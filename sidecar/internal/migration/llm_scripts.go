package migration

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/llm"
)

// scriptLLMTimeout is the context deadline for migration script generation.
const scriptLLMTimeout = 30 * time.Second

// scriptMaxTokens is the max tokens for migration script generation.
const scriptMaxTokens = 4096

// ScriptGenerator produces tailored migration SQL scripts using LLM.
type ScriptGenerator struct {
	llmClient *llm.Client
	pool      *pgxpool.Pool
	pgVersion int
	logFn     func(string, string, ...any)
}

// NewScriptGenerator creates a ScriptGenerator. If llmClient is nil,
// Generate always returns the deterministic SafeAlternative.
func NewScriptGenerator(
	llmClient *llm.Client,
	pool *pgxpool.Pool,
	pgVersion int,
	logFn func(string, string, ...any),
) *ScriptGenerator {
	return &ScriptGenerator{
		llmClient: llmClient,
		pool:      pool,
		pgVersion: pgVersion,
		logFn:     logFn,
	}
}

// Generate produces a tailored migration script for the given risk.
// Falls back to risk.SafeAlternative when the LLM is unavailable.
func (g *ScriptGenerator) Generate(
	ctx context.Context, risk *DDLRisk,
) (string, error) {
	if g.llmClient == nil || !g.llmClient.IsEnabled() {
		return risk.SafeAlternative, nil
	}

	schema := g.fetchTableSchema(ctx, risk.SchemaName, risk.TableName)
	system := scriptSystemPrompt()
	user := scriptUserPrompt(risk, schema, g.pgVersion)

	ctx, cancel := context.WithTimeout(ctx, scriptLLMTimeout)
	defer cancel()

	raw, _, err := g.llmClient.Chat(ctx, system, user, scriptMaxTokens)
	if err != nil {
		g.logFn("warn",
			"migration: LLM script generation failed: %v", err)
		return risk.SafeAlternative, nil
	}

	script := extractSQLFromResponse(raw)
	if script == "" {
		return risk.SafeAlternative, nil
	}
	return script, nil
}

// fetchTableSchema queries pg_attribute and pg_constraint for a
// human-readable description of the table. Returns empty string on
// any error (non-fatal).
func (g *ScriptGenerator) fetchTableSchema(
	ctx context.Context, schema, table string,
) string {
	if g.pool == nil || table == "" {
		return ""
	}
	if schema == "" {
		schema = "public"
	}

	result, err := queryTableSchema(ctx, g.pool, schema, table)
	if err != nil {
		g.logFn("warn",
			"migration: schema query for %s.%s failed: %v",
			schema, table, err)
		return ""
	}
	return result
}

// queryTableSchema fetches column definitions and constraints from
// the catalog for prompt context.
func queryTableSchema(
	ctx context.Context, pool *pgxpool.Pool,
	schema, table string,
) (string, error) {
	cols, err := queryColumns(ctx, pool, schema, table)
	if err != nil {
		return "", fmt.Errorf("columns: %w", err)
	}

	constraints, err := queryConstraints(ctx, pool, schema, table)
	if err != nil {
		return "", fmt.Errorf("constraints: %w", err)
	}

	indexes, err := queryIndexes(ctx, pool, schema, table)
	if err != nil {
		return "", fmt.Errorf("indexes: %w", err)
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("-- Table: %s.%s\n", schema, table))
	if cols != "" {
		b.WriteString("-- Columns:\n")
		b.WriteString(cols)
	}
	if constraints != "" {
		b.WriteString("-- Constraints:\n")
		b.WriteString(constraints)
	}
	if indexes != "" {
		b.WriteString("-- Indexes:\n")
		b.WriteString(indexes)
	}
	return b.String(), nil
}

const columnQuery = `
SELECT a.attname,
       pg_catalog.format_type(a.atttypid, a.atttypmod),
       CASE WHEN a.attnotnull THEN 'NOT NULL' ELSE '' END,
       COALESCE(pg_catalog.pg_get_expr(d.adbin, d.adrelid), '')
FROM   pg_catalog.pg_attribute a
LEFT JOIN pg_catalog.pg_attrdef d
       ON d.adrelid = a.attrelid AND d.adnum = a.attnum
WHERE  a.attrelid = ($1 || '.' || $2)::regclass
  AND  a.attnum > 0
  AND  NOT a.attisdropped
ORDER BY a.attnum`

// queryColumns returns a formatted column listing.
func queryColumns(
	ctx context.Context, pool *pgxpool.Pool,
	schema, table string,
) (string, error) {
	rows, err := pool.Query(ctx, columnQuery, schema, table)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var name, typ, notNull, defVal string
		if err := rows.Scan(&name, &typ, &notNull, &defVal); err != nil {
			return "", err
		}
		line := fmt.Sprintf("--   %s %s", name, typ)
		if notNull != "" {
			line += " " + notNull
		}
		if defVal != "" {
			line += " DEFAULT " + defVal
		}
		b.WriteString(line + "\n")
	}
	return b.String(), rows.Err()
}

const constraintQuery = `
SELECT conname,
       pg_catalog.pg_get_constraintdef(c.oid, true)
FROM   pg_catalog.pg_constraint c
WHERE  c.conrelid = ($1 || '.' || $2)::regclass
ORDER BY conname`

// queryConstraints returns a formatted constraint listing.
func queryConstraints(
	ctx context.Context, pool *pgxpool.Pool,
	schema, table string,
) (string, error) {
	rows, err := pool.Query(ctx, constraintQuery, schema, table)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			return "", err
		}
		b.WriteString(fmt.Sprintf("--   %s: %s\n", name, def))
	}
	return b.String(), rows.Err()
}

const indexQuery = `
SELECT indexname, indexdef
FROM   pg_indexes
WHERE  schemaname = $1
  AND  tablename  = $2
ORDER BY indexname`

// queryIndexes returns a formatted index listing.
func queryIndexes(
	ctx context.Context, pool *pgxpool.Pool,
	schema, table string,
) (string, error) {
	rows, err := pool.Query(ctx, indexQuery, schema, table)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			return "", err
		}
		b.WriteString(fmt.Sprintf("--   %s\n", def))
	}
	return b.String(), rows.Err()
}

// scriptSystemPrompt returns the system message for migration script
// generation.
func scriptSystemPrompt() string {
	return `You are a PostgreSQL migration expert. Generate a safe, ` +
		`production-ready migration script as an alternative to a ` +
		`dangerous DDL statement.

Requirements:
- Use transactions where appropriate
- Include SET lock_timeout = '5s' before any locking operation
- Use CREATE INDEX CONCURRENTLY (outside transactions) where needed
- Add NOT VALID + VALIDATE CONSTRAINT pattern for constraints
- For type changes: new column, backfill in batches, swap columns
- Include comments explaining each step
- Return ONLY the SQL script, no explanations outside the SQL`
}

// scriptUserPrompt builds the user prompt with risk context and
// table schema for a specific DDL migration.
func scriptUserPrompt(
	risk *DDLRisk, tableSchema string, pgVersion int,
) string {
	schemaName := risk.SchemaName
	if schemaName == "" {
		schemaName = "public"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "PostgreSQL version: %d\n", pgVersion)
	fmt.Fprintf(&b, "Original DDL (dangerous):\n%s\n\n", risk.Statement)
	fmt.Fprintf(&b, "Risk: %s -- %s\n", risk.RuleID, risk.Description)
	fmt.Fprintf(&b, "Lock level: %s\n", risk.LockLevel)
	fmt.Fprintf(&b, "Table: %s.%s (%d rows, %d bytes)\n\n",
		schemaName, risk.TableName,
		risk.EstimatedRows, risk.TableSizeBytes)

	if tableSchema != "" {
		fmt.Fprintf(&b, "Table schema:\n%s\n", tableSchema)
	}

	if risk.SafeAlternative != "" {
		fmt.Fprintf(&b, "Safe alternative hint: %s\n\n",
			risk.SafeAlternative)
	}

	b.WriteString("Generate a complete, safe migration script.")
	return b.String()
}

// extractSQLFromResponse extracts SQL from an LLM response that may
// be wrapped in markdown code fences or contain surrounding prose.
func extractSQLFromResponse(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}

	// Check for markdown code fences.
	if idx := strings.Index(s, "```"); idx >= 0 {
		return extractFromFences(s)
	}

	// No fences — treat the entire response as SQL if it looks like
	// it contains SQL statements.
	if looksLikeSQL(s) {
		return s
	}
	return s
}

// extractFromFences pulls content from the first markdown code
// fence block.
func extractFromFences(s string) string {
	start := strings.Index(s, "```")
	if start < 0 {
		return s
	}

	// Skip the opening fence line (```sql, ```pgsql, etc.)
	afterFence := s[start+3:]
	if nl := strings.Index(afterFence, "\n"); nl >= 0 {
		afterFence = afterFence[nl+1:]
	}

	// Find closing fence.
	end := strings.Index(afterFence, "```")
	if end >= 0 {
		afterFence = afterFence[:end]
	}

	return strings.TrimSpace(afterFence)
}

// looksLikeSQL returns true if the text contains common SQL keywords
// suggesting it is executable SQL.
func looksLikeSQL(s string) bool {
	upper := strings.ToUpper(s)
	keywords := []string{
		"BEGIN", "SET ", "ALTER ", "CREATE ", "DROP ",
		"INSERT ", "UPDATE ", "DELETE ", "SELECT ",
		"COMMIT", "ROLLBACK",
	}
	for _, kw := range keywords {
		if strings.Contains(upper, kw) {
			return true
		}
	}
	return false
}
