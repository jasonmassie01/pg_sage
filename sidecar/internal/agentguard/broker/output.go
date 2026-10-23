package broker

import (
	"context"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/agentguard/classify"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// maxViewDepth matches the catalog proof: views are followed four deep.
const maxViewDepth = 4

// classifier answers "what class is this column" for one call, caching
// each relation's classes and following views to their base columns.
type classifier struct {
	b      *Broker
	t      Target
	pid    string
	env    envbind.Env
	cache  map[uint32]classify.RelationClasses
	views  map[uint32]classify.Class
	kinds  map[uint32]string
	names  map[uint32]map[int16]string
	errOut error
}

func (c *classifier) relClasses(ctx context.Context, relid uint32) classify.RelationClasses {
	if rc, ok := c.cache[relid]; ok {
		return rc
	}
	rc, err := c.b.deps.Classes.Lookup(ctx, c.t, relid)
	if err != nil && c.errOut == nil {
		c.errOut = err
	}
	c.cache[relid] = rc
	return rc
}

// unmasked reports an agents.unmask entry for the column.
func (c *classifier) unmasked(relid uint32, attnum int16) bool {
	if c.b.deps.Unmasked == nil {
		return false
	}
	col := classify.Column{RelID: relid, AttNum: attnum, Name: c.names[relid][attnum]}
	return c.b.deps.Unmasked(c.pid, c.t.DatabaseID, col)
}

// columnClass is a column's binding class: its own (or its table's) and,
// for a view, the narrowest class of the base columns the view reads.
func (c *classifier) columnClass(ctx context.Context, relid uint32, attnum int16) classify.Class {
	cls := c.relClasses(ctx, relid).Of(attnum).Class
	if c.kinds[relid] == "v" {
		cls = narrower(cls, c.viewColumnClass(ctx, relid, attnum))
	}
	return cls
}

// viewColumnClass is the class a view column carries from its base
// columns. In stage and prod, where the class is a control, every column
// of a view takes the narrowest class the view reads (a view can rename a
// column). In branch and dev, where masking is a convenience, a column
// named like a base column it reads takes that column's class, and any
// other column the view's narrowest.
func (c *classifier) viewColumnClass(ctx context.Context, view uint32,
	attnum int16) classify.Class {
	whole := c.viewClass(ctx, view, 0)
	if !classify.ShouldMask(c.env, classify.ClassPII, false) {
		return whole
	}
	name := c.names[view][attnum]
	deps, err := viewDependencies(ctx, c.t.Pool, view)
	if err != nil {
		c.errOut = err
		return classify.ClassSecret
	}
	matched, cls := false, classify.Unclassified
	for _, d := range deps {
		if d.attnum == 0 || d.kind == "v" || c.baseName(ctx, d.relid, d.attnum) != name {
			continue
		}
		matched = true
		cls = narrower(cls, c.relClasses(ctx, d.relid).Of(d.attnum).Class)
	}
	if !matched {
		return whole
	}
	return cls
}

// baseName is a base column's name, loading the relation's names once.
func (c *classifier) baseName(ctx context.Context, relid uint32, attnum int16) string {
	if _, ok := c.names[relid]; !ok {
		names, err := attributeNames(ctx, c.t.Pool, []uint32{relid})
		if err != nil {
			c.errOut = err
		}
		c.names[relid] = names[relid]
	}
	return c.names[relid][attnum]
}

// viewClass is the narrowest class among the base columns a view reads.
func (c *classifier) viewClass(ctx context.Context, view uint32, depth int) classify.Class {
	if cls, ok := c.views[view]; ok {
		return cls
	}
	if depth >= maxViewDepth {
		return classify.ClassSecret // deeper than followed: assume the worst
	}
	deps, err := viewDependencies(ctx, c.t.Pool, view)
	if err != nil {
		c.errOut = err
		return classify.ClassSecret
	}
	cls := classify.Unclassified
	for _, d := range deps {
		cls = narrower(cls, c.relClasses(ctx, d.relid).Of(d.attnum).Class)
		if d.attnum == 0 {
			cls = narrower(cls, sensitiveOf(c.relClasses(ctx, d.relid)))
		}
		if d.kind == "v" {
			cls = narrower(cls, c.viewClass(ctx, d.relid, depth+1))
		}
	}
	c.views[view] = cls
	return cls
}

// sensitiveOf is the narrowest pii or secret class of any column of rc.
func sensitiveOf(rc classify.RelationClasses) classify.Class {
	cls := classify.Unclassified
	for attnum := range rc.Columns {
		if e := rc.Of(attnum).Class; e == classify.ClassPII || e == classify.ClassSecret {
			cls = narrower(cls, e)
		}
	}
	return cls
}

func narrower(a, b classify.Class) classify.Class {
	if b.Narrowness() > a.Narrowness() {
		return b
	}
	return a
}

// outputPlan is what happens to each output column.
type outputPlan struct {
	columns []Column
	actions []columnAction
	masked  []string
	classes []string
	denied  []string
}

// planOutput decides every output column (§6.8 S5). A column with a table
// origin takes its own class; an expression (table OID 0) counts as
// classified whenever the statement reads a relation with a pii or secret
// column. maskedRef refuses statements that use a masked column outside
// the output.
func (c *classifier) planOutput(ctx context.Context, fields []pgconn.FieldDescription,
	rels []relation, types map[uint32]string) outputPlan {
	refClass := classify.Unclassified
	for _, r := range rels {
		if r.OID != 0 {
			refClass = narrower(refClass, c.relationSensitivity(ctx, r))
		}
	}
	var p outputPlan
	classes := map[string]bool{}
	for _, f := range fields {
		cls := refClass
		unmasked := false
		if f.TableOID != 0 {
			cls = c.columnClass(ctx, f.TableOID, int16(f.TableAttributeNumber))
			unmasked = c.unmasked(f.TableOID, int16(f.TableAttributeNumber))
		}
		act := decideColumn(c.env, cls, unmasked)
		if cls != classify.Unclassified && cls != classify.ClassClean {
			classes[string(cls)] = true
		}
		p.add(f.Name, types[f.DataTypeOID], cls, act)
	}
	p.classes = sortedKeys(classes)
	return p
}

func (p *outputPlan) add(name, typ string, cls classify.Class, act columnAction) {
	if typ == "" {
		typ = "unknown"
	}
	p.columns = append(p.columns, Column{Name: name, Type: typ, Class: string(cls)})
	p.actions = append(p.actions, act)
	switch act {
	case actMask:
		p.masked = append(p.masked, name)
	case actDeny:
		p.denied = append(p.denied, name)
	}
}

// relationSensitivity is the narrowest pii or secret class a relation
// exposes, ignoring columns an unmask entry covers.
func (c *classifier) relationSensitivity(ctx context.Context, r relation) classify.Class {
	cls := classify.Unclassified
	rc := c.relClasses(ctx, r.OID)
	for attnum := range rc.Columns {
		e := rc.Of(attnum).Class
		if (e == classify.ClassPII && !c.unmasked(r.OID, attnum)) || e == classify.ClassSecret {
			cls = narrower(cls, e)
		}
	}
	if r.Kind == "v" {
		v := c.viewClass(ctx, r.OID, 0)
		if v == classify.ClassPII || v == classify.ClassSecret {
			cls = narrower(cls, v)
		}
	}
	return cls
}

// maskedRefs refuses a statement that uses a masked column of a referenced
// relation outside its bare output (G1-15: a cast on a masked column).
func (c *classifier) maskedRefs(ctx context.Context, rels []relation,
	read sqlast.BrokeredRead) string {
	masked, known := map[string]bool{}, map[string]bool{}
	for _, r := range rels {
		for attnum, name := range c.names[r.OID] {
			known[name] = true
			cls := c.columnClass(ctx, r.OID, attnum)
			if decideColumn(c.env, cls, c.unmasked(r.OID, attnum)) == actMask {
				masked[name] = true
			}
		}
	}
	return maskedRefRefusal(masked, known, read.OtherColumnRefs)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// deniedDetail names the refused columns.
func (p outputPlan) deniedDetail() string {
	return "the result would include " + strings.Join(p.denied, ", ") +
		", classified columns an agent may not read in this environment"
}
