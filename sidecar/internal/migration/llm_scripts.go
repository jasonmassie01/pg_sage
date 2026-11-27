package migration

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/catalogread"
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
	timeouts  catalogread.Timeouts // bound each schema read
	cache     *replyCache          // nil: every call asks the model

	// schemaText describes a table for the prompt and the cache key; a
	// seam for tests, fetchTableSchema in production.
	schemaText func(ctx context.Context, schema, table string) string
}

// NewScriptGenerator creates a ScriptGenerator. If llmClient is nil,
// Generate always returns the deterministic SafeAlternative.
func NewScriptGenerator(
	llmClient *llm.Client,
	pool *pgxpool.Pool,
	pgVersion int,
	logFn func(string, string, ...any),
) *ScriptGenerator {
	g := &ScriptGenerator{
		llmClient: llmClient,
		pool:      pool,
		pgVersion: pgVersion,
		logFn:     logFn,
		timeouts:  catalogread.Default(),
	}
	g.schemaText = g.fetchTableSchema
	return g
}

// ScriptResult is a migration recommendation. SQL is always the
// deterministic alternative: model text is never the recommendation
// (migration-10). Script is the model's script once validated; ModelText
// is the script pg_sage could not validate, and Reason says why. Both are
// shown as model output (security-05).
type ScriptResult struct {
	SQL       string
	Script    string
	ModelText string
	Reason    string
}

// Generate returns the recommended script for the given risk, which is
// risk.SafeAlternative.
func (g *ScriptGenerator) Generate(
	ctx context.Context, risk *DDLRisk,
) (string, error) {
	return g.GenerateScript(ctx, risk).SQL, nil
}

// GenerateScript asks the model for a tailored migration script, through
// the reply cache, and keeps it as Script only when validateModelDDL
// accepts it for the risk's table.
func (g *ScriptGenerator) GenerateScript(
	ctx context.Context, risk *DDLRisk,
) ScriptResult {
	out := ScriptResult{SQL: risk.SafeAlternative}
	if g.llmClient == nil || !g.llmClient.IsEnabled() {
		return out
	}
	schema := g.schemaText(ctx, risk.SchemaName, risk.TableName)
	reply, ok := g.reply(ctx, risk, schema)
	script := extractSQLFromResponse(reply)
	if !ok || script == "" {
		return out
	}
	target := modelTarget{Schema: risk.SchemaName, Table: risk.TableName}
	if err := validateModelDDL(script, target); err != nil {
		g.logFn("warn", "migration: model script for %s not validated: %v",
			qualifiedName(risk.SchemaName, risk.TableName), err)
		out.ModelText, out.Reason = script, err.Error()
		return out
	}
	out.Script = script
	return out
}

// reply is the model's answer for risk on this schema: cached for the
// statement's identity, else asked. A failed call, and a reply with no
// SQL in it, is not cached, so the next detection asks again.
func (g *ScriptGenerator) reply(
	ctx context.Context, risk *DDLRisk, schema string,
) (string, bool) {
	key := enrichKey(risk.RuleID, risk.Statement, schema)
	if raw, ok := g.cache.get(key); ok {
		return raw, true
	}
	raw, err := g.modelScript(ctx, risk, schema)
	if err != nil {
		g.logFn("warn", "migration: LLM script generation failed: %v", err)
		return "", false
	}
	if extractSQLFromResponse(raw) != "" {
		g.cache.put(key, raw)
	}
	return raw, true
}

// modelScript asks the model for a script for risk on the given schema.
func (g *ScriptGenerator) modelScript(
	ctx context.Context, risk *DDLRisk, schema string,
) (string, error) {
	system := scriptSystemPrompt()
	user := scriptUserPrompt(risk, schema, g.pgVersion)

	ctx, cancel := context.WithTimeout(ctx, scriptLLMTimeout)
	defer cancel()

	pctx := llm.WithPurpose(ctx, "migration_script") // llm-04: usage ledger label
	raw, _, err := g.llmClient.Chat(pctx, system, user, scriptMaxTokens)
	return raw, err
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
- Return ONLY the SQL script, no explanations outside the SQL

` + llm.UntrustedDataRule
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
	fmt.Fprintf(&b, "PostgreSQL version: %s\n", formatPGVersion(pgVersion))
	fmt.Fprintf(&b, "Original DDL (dangerous):\n%s\n\n",
		llm.UntrustedData("ddl", sanitizeDDL(risk.Statement)))
	fmt.Fprintf(&b, "Risk: %s -- %s\n", risk.RuleID, risk.Description)
	fmt.Fprintf(&b, "Lock level: %s\n", risk.LockLevel)
	fmt.Fprintf(&b, "Table (%d rows, %d bytes):\n%s\n\n",
		risk.EstimatedRows, risk.TableSizeBytes,
		llm.UntrustedData("table", schemaName+"."+risk.TableName))

	if tableSchema != "" {
		// Column defaults, CHECK bodies and index predicates can hold
		// literals; redact them before egress (G7-B01).
		fmt.Fprintf(&b, "Table schema:\n%s\n",
			llm.UntrustedData("table_schema", redactSchemaLiterals(tableSchema)))
	}

	if risk.SafeAlternative != "" {
		fmt.Fprintf(&b, "Safe alternative hint: %s\n\n",
			risk.SafeAlternative)
	}

	b.WriteString("Generate a complete, safe migration script.")
	return b.String()
}

// redactSchemaLiterals redacts literals in the catalog-derived schema
// description line by line, so a "--" prefix does not swallow a line.
func redactSchemaLiterals(schema string) string {
	lines := strings.Split(schema, "\n")
	for i, line := range lines {
		body := strings.TrimPrefix(line, "--")
		if body != line {
			lines[i] = "--" + sanitizeDDL(body)
		}
	}
	return strings.Join(lines, "\n")
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

	// No fences: the entire response is treated as the script.
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
