package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/config"
)

// Catalog and documentation reads: one table's shape, a configuration
// key and a pg_sage concept.

var (
	qualifiedTable = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]{0,62}\.` +
		`[A-Za-z_][A-Za-z0-9_$]{0,62}$`)
	configKey     = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_]+){0,4}$`)
	systemSchemas = map[string]bool{"pg_catalog": true, "information_schema": true,
		"pg_toast": true}
)

func (ss *session) catalogTools() []agentloop.Tool {
	return []agentloop.Tool{
		tool("describe_table", "Describe one table: size, estimated rows, columns, "+
			"indexes, its comment and vacuum/analyze times.", `"table":{"type":"string",`+
			`"description":"schema-qualified name, e.g. public.orders","maxLength":130}`,
			[]string{"table"}, ss.describeTable),
		tool("explain_config", "Explain one pg_sage configuration key: what it does, its "+
			"current value (never secrets) and whether a change needs a restart.",
			`"key":{"type":"string","description":"YAML path, e.g. ask.retention_days",`+
				`"maxLength":100}`, []string{"key"}, ss.explainConfig),
		tool("explain_concept", "Explain a pg_sage concept: "+strings.Join(
			ConceptTopics(), ", ")+".", `"topic":{"type":"string","enum":`+
			conceptEnum()+`}`, []string{"topic"}, ss.explainConcept),
	}
}

func (ss *session) describeTable(ctx context.Context, raw json.RawMessage) (
	agentloop.Output, error) {
	var a struct {
		Table string `json:"table"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return agentloop.Output{}, err
	}
	if !qualifiedTable.MatchString(a.Table) {
		return agentloop.Output{}, invalidArgs("table must be schema.name")
	}
	schema, name, _ := strings.Cut(a.Table, ".")
	if systemSchemas[strings.ToLower(schema)] {
		return agentloop.Output{}, invalidArgs("system catalogs are not described")
	}
	id := "table:" + a.Table
	text, err := ss.tableText(ctx, schema, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return ss.cite(id, "not_found", "table "+a.Table+" does not exist"), nil
	}
	if err != nil {
		return agentloop.Output{}, fmt.Errorf("describe %s: %w", a.Table, err)
	}
	return ss.cite(id, "ok", text), nil
}

func (ss *session) tableText(ctx context.Context, schema, name string) (string, error) {
	var size, rows int64
	var comment, columns, indexes, vacuum, analyze string
	var ncols int
	err := ss.s.d.Pool.QueryRow(ctx, `/* pg_sage */ SELECT pg_total_relation_size(c.oid),
		c.reltuples::bigint, COALESCE(obj_description(c.oid, 'pg_class'), ''),
		(SELECT count(*) FROM pg_attribute a WHERE a.attrelid = c.oid AND a.attnum > 0
		   AND NOT a.attisdropped),
		COALESCE((SELECT string_agg(a.attname || ' ' || format_type(a.atttypid,
		   a.atttypmod), ', ' ORDER BY a.attnum) FROM pg_attribute a
		   WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped), ''),
		COALESCE((SELECT string_agg(pg_get_indexdef(i.indexrelid), '; ' ORDER BY
		   i.indexrelid) FROM pg_index i WHERE i.indrelid = c.oid), ''),
		COALESCE(GREATEST(s.last_vacuum, s.last_autovacuum)::text, 'never'),
		COALESCE(GREATEST(s.last_analyze, s.last_autoanalyze)::text, 'never')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_stat_user_tables s ON s.relid = c.oid
		WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN ('r', 'p', 'm')`,
		schema, name).Scan(&size, &rows, &comment, &ncols, &columns, &indexes, &vacuum,
		&analyze)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "table %s.%s: %d bytes total, about %d rows, %d columns\n", schema,
		name, size, max(rows, 0), ncols)
	optional(&b, "comment", oneLine(comment))
	fmt.Fprintf(&b, "columns: %s\n", clip(columns, 1500))
	optional(&b, "indexes", clip(indexes, 1500))
	fmt.Fprintf(&b, "last vacuum %s, last analyze %s", vacuum, analyze)
	return b.String(), nil
}

func (ss *session) explainConfig(_ context.Context, raw json.RawMessage) (agentloop.Output,
	error) {
	var a struct {
		Key string `json:"key"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return agentloop.Output{}, err
	}
	if !configKey.MatchString(a.Key) {
		return agentloop.Output{}, invalidArgs("key must be a YAML path like ask.enabled")
	}
	id := "config:" + a.Key
	doc, ok := config.DescribeField(ss.s.d.Settings, a.Key)
	if !ok {
		return ss.cite(id, "not_found", "pg_sage has no configuration key "+a.Key), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\nlifecycle: %s", doc.Path, oneLine(doc.Doc), doc.Lifecycle)
	if doc.Lifecycle == string(config.LifecycleRestart) {
		b.WriteString(" (a change needs a restart)")
	}
	if w := oneLine(doc.Warning); w != "" {
		b.WriteString("\nwarning: " + w)
	}
	switch {
	case doc.HasValue:
		raw, err := json.Marshal(doc.Value)
		if err != nil {
			return agentloop.Output{}, fmt.Errorf("encode %s: %w", a.Key, err)
		}
		fmt.Fprintf(&b, "\ncurrent value: %s", raw)
	default:
		b.WriteString("\ncurrent value: not shown (secret or may carry a credential)")
	}
	return ss.cite(id, "ok", b.String()), nil
}

func (ss *session) explainConcept(_ context.Context, raw json.RawMessage) (
	agentloop.Output, error) {
	var a struct {
		Topic string `json:"topic"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return agentloop.Output{}, err
	}
	text, ok := concepts[a.Topic]
	if !ok {
		return agentloop.Output{}, invalidArgs("unknown topic %q", a.Topic)
	}
	return ss.cite("doc:"+a.Topic, "ok", text), nil
}
