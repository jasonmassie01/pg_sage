package analyzer

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sanitize"
	"github.com/pg-sage/sidecar/internal/sqlident"
)

// partmanMaxParents bounds the part_config parents read per cycle.
const partmanMaxParents = 100

// partmanReading is one cycle's pg_partman reading.
type partmanReading struct {
	Schema      string
	Parents     []partmanParent
	premakeRead bool
	defaultRead bool
}

// partmanParent is one part_config parent. FutureRead and DefaultRead say
// whether its children and its default partition were read this cycle.
type partmanParent struct {
	Parent         string // quoted schema.table
	ConfigName     string // part_config.parent_table as pg_partman stores it
	Control        string
	Kind           string // "time" or "id"
	Premake        int
	Future         int
	FutureRead     bool
	Default        string // quoted default partition, "" without one
	DefaultRows    float64
	DefaultHasRows bool
	DefaultRead    bool
}

// partmanChild is one child, quoted, with its lower bound from
// show_partition_info.
type partmanChild struct {
	Table   string
	Start   *time.Time
	StartID *int64
}

// partmanConfigSQL lists the managed parents that exist, with their
// default partition from the catalog, at most $1.
func partmanConfigSQL(schema string) string {
	return `/* pg_sage analyzer */
SELECT pc.parent_table, pc.control, pc.premake, pn.nspname, pcl.relname,
       dn.nspname, d.relname, COALESCE(d.reltuples::float8, -1)
  FROM ` + sqlident.Qualified(schema, "part_config") + ` pc
  JOIN pg_class pcl ON pcl.oid = to_regclass(pc.parent_table)
  JOIN pg_namespace pn ON pn.oid = pcl.relnamespace
  LEFT JOIN LATERAL (
       SELECT c.relname, c.relnamespace, c.reltuples
         FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
        WHERE i.inhparent = pcl.oid AND c.relispartition
          AND pg_get_expr(c.relpartbound, c.oid) = 'DEFAULT'
        LIMIT 1) d ON true
  LEFT JOIN pg_namespace dn ON dn.oid = d.relnamespace
 ORDER BY pc.parent_table
 LIMIT $1`
}

func readPartmanConfig(ctx context.Context, q catalogread.Querier, schema string) (
	[]partmanParent, error,
) {
	rows, err := q.Query(ctx, partmanConfigSQL(schema), partmanMaxParents+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []partmanParent
	for rows.Next() {
		var p partmanParent
		var nsp, rel string
		var defNsp, defRel *string
		if err := rows.Scan(&p.ConfigName, &p.Control, &p.Premake, &nsp, &rel,
			&defNsp, &defRel, &p.DefaultRows); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		p.Parent = sqlident.Qualified(nsp, rel)
		if defNsp != nil && defRel != nil {
			p.Default = sqlident.Qualified(*defNsp, *defRel)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// readPartman reads pg_partman's parents in schema; what it could not
// read is not evaluated, or kept open per parent.
func (a *Analyzer) readPartman(ctx context.Context, q catalogread.Querier,
	schema string) *partmanReading {
	r := &partmanReading{Schema: schema}
	parents, err := readPartmanConfig(ctx, q, schema)
	if err != nil {
		a.extWarn("partman_config", "analyzer: pg_partman: read %s: %v; pg_partman "+
			"checks not evaluated", sqlident.Qualified(schema, "part_config"), err)
		a.evalFail(partmanCategories...)
		return r
	}
	a.extOK("partman_config")
	if len(parents) > partmanMaxParents {
		// The parents past the limit may hold open findings.
		parents = parents[:partmanMaxParents]
		a.evalFail(partmanCategories...)
	}
	premakeOn := a.extRulesOn([]string{categoryPartmanPremake})
	defaultOn := a.extRulesOn([]string{categoryPartmanDefault})
	for i := range parents {
		if premakeOn {
			a.readPartmanFuture(ctx, q, schema, &parents[i])
		}
		if defaultOn {
			a.readPartmanDefault(ctx, q, &parents[i])
		}
	}
	r.Parents, r.premakeRead, r.defaultRead = parents, true, true
	return r
}

// readPartmanFuture counts the parent's future children among its newest
// premake+1.
func (a *Analyzer) readPartmanFuture(ctx context.Context, q catalogread.Querier,
	schema string, p *partmanParent) {
	if p.Premake <= 0 {
		return
	}
	key := "partman_children:" + p.Parent
	children, err := readPartmanChildren(ctx, q, schema, p.ConfigName, p.Premake+1)
	var maxID *int64
	if err == nil && childKind(children) == "id" {
		maxID, err = readPartmanMaxID(ctx, q, children, p.Control)
	}
	if err != nil {
		a.extWarn(key, "analyzer: pg_partman: read the children of %s: %v; its %s "+
			"finding is kept open", p.Parent, err, categoryPartmanPremake)
		a.extras.holdOpen(categoryPartmanPremake, p.Parent)
		return
	}
	a.extOK(key)
	p.Future, p.Kind = countFuturePartitions(children, maxID, a.extras.now())
	p.FutureRead = true
}

func readPartmanChildren(ctx context.Context, q catalogread.Querier, schema, parent string,
	limit int) ([]partmanChild, error) {
	s := sqlident.Ident(schema)
	rows, err := q.Query(ctx, `/* pg_sage analyzer */
SELECT p.partition_schemaname, p.partition_tablename,
       i.child_start_time, i.child_start_id
  FROM `+s+`.show_partitions($1, 'DESC')
       WITH ORDINALITY AS p(partition_schemaname, partition_tablename, n)
  CROSS JOIN LATERAL `+s+`.show_partition_info(
       p.partition_schemaname || '.' || p.partition_tablename, NULL::text, $1) i
 ORDER BY p.n
 LIMIT $2`, parent, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []partmanChild
	for rows.Next() {
		var c partmanChild
		var nsp, rel string
		if err := rows.Scan(&nsp, &rel, &c.Start, &c.StartID); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		c.Table = sqlident.Qualified(nsp, rel)
		out = append(out, c)
	}
	return out, rows.Err()
}

// readPartmanMaxID reads the newest control value of an id-controlled
// parent from its listed children, newest first: ranges do not overlap, so
// the first child holding a row holds the maximum. It reads no older child
// and never the whole parent; nil when no listed child holds a row, and
// then every listed child is future.
func readPartmanMaxID(ctx context.Context, q catalogread.Querier, children []partmanChild,
	control string) (*int64, error) {
	for _, c := range children {
		var v *int64
		err := q.QueryRow(ctx, "/* pg_sage analyzer */ SELECT max("+sqlident.Ident(control)+
			")::bigint FROM "+c.Table).Scan(&v)
		if err != nil {
			return nil, fmt.Errorf("read max(%s) of %s: %w", control, c.Table, err)
		}
		if v != nil {
			return v, nil
		}
	}
	return nil, nil
}

// childKind is "time" when the children have time bounds, "id" when they
// have id bounds, and "" without children.
func childKind(children []partmanChild) string {
	for _, c := range children {
		if c.Start != nil {
			return "time"
		}
		if c.StartID != nil {
			return "id"
		}
	}
	return ""
}

// countFuturePartitions counts the children that start after now (time)
// or after the newest id (id); with no row every id child is future.
func countFuturePartitions(children []partmanChild, maxID *int64, now time.Time) (int, string) {
	kind := childKind(children)
	n := 0
	for _, c := range children {
		switch {
		case kind == "time" && c.Start != nil && c.Start.After(now):
			n++
		case kind == "id" && c.StartID != nil && (maxID == nil || *c.StartID > *maxID):
			n++
		}
	}
	return n, kind
}

// readPartmanDefault checks whether the default partition holds a row.
func (a *Analyzer) readPartmanDefault(ctx context.Context, q catalogread.Querier,
	p *partmanParent) {
	if p.Default == "" {
		return
	}
	key := "partman_default:" + p.Default
	err := q.QueryRow(ctx, "/* pg_sage analyzer */ SELECT EXISTS (SELECT 1 FROM "+
		p.Default+")").Scan(&p.DefaultHasRows)
	if err != nil {
		a.extWarn(key, "analyzer: pg_partman: read %s: %v; its %s finding is kept open",
			p.Default, err, categoryPartmanDefault)
		a.extras.holdOpen(categoryPartmanDefault, p.Default)
		return
	}
	a.extOK(key)
	p.DefaultRead = true
}

// rulePartmanPremakeGap reports parents with fewer future children than
// premake.
func rulePartmanPremakeGap(_, _ *collector.Snapshot, _ *config.Config,
	extras *RuleExtras) []Finding {
	r := extras.partmanReading()
	if r == nil || !r.premakeRead {
		return nil
	}
	var out []Finding
	for _, p := range r.Parents {
		if !p.FutureRead || p.Premake <= 0 || p.Future >= p.Premake {
			continue
		}
		out = append(out, Finding{
			Category:         categoryPartmanPremake,
			Severity:         "warning",
			ObjectType:       "table",
			ObjectIdentifier: p.Parent,
			Title: fmt.Sprintf("pg_partman parent %s has %d future partitions, "+
				"fewer than premake %d", p.Parent, p.Future, p.Premake),
			Detail: map[string]any{"parent_table": p.Parent, "premake": p.Premake,
				"future_partitions": p.Future, "control": p.Control,
				"control_kind": p.Kind, "schema": r.Schema},
			Recommendation: fmt.Sprintf("CALL %s.run_maintenance_proc() creates the "+
				"missing children. Then find why maintenance stopped: pg_partman's "+
				"background worker (pg_partman_bgw.interval), the job that calls it, or "+
				"automatic_maintenance off for %s. Advisory only.",
				sqlident.Ident(r.Schema), sanitize.QuoteLiteral(p.ConfigName)),
		})
	}
	return out
}

// rulePartmanDefaultRows reports default partitions that hold rows.
func rulePartmanDefaultRows(_, _ *collector.Snapshot, _ *config.Config,
	extras *RuleExtras) []Finding {
	r := extras.partmanReading()
	if r == nil || !r.defaultRead {
		return nil
	}
	var out []Finding
	for _, p := range r.Parents {
		if !p.DefaultRead || !p.DefaultHasRows {
			continue
		}
		out = append(out, Finding{
			Category:         categoryPartmanDefault,
			Severity:         "warning",
			ObjectType:       "table",
			ObjectIdentifier: p.Default,
			Title:            fmt.Sprintf("Rows landed in the default partition %s", p.Default),
			Detail: map[string]any{"parent_table": p.Parent, "default_partition": p.Default,
				"estimated_rows": int64(max(p.DefaultRows, 0)), "schema": r.Schema},
			Recommendation: fmt.Sprintf("These rows match no child partition, and "+
				"pg_partman cannot create a child whose range they fall in. CALL "+
				"%s.partition_data_proc(%s) moves them into proper children (pg_partman "+
				"5; partition_data_time() or partition_data_id() on 4.x). Advisory only.",
				sqlident.Ident(r.Schema), sanitize.QuoteLiteral(p.ConfigName)),
		})
	}
	return out
}

func (e *RuleExtras) partmanReading() *partmanReading {
	if e == nil || e.ext == nil {
		return nil
	}
	return e.ext.partman
}
