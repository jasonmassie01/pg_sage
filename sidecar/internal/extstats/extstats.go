// Package extstats parses the one CREATE STATISTICS form pg_sage runs and
// its inverse (owner decision 2026-10-04, PR #110): a pg_sage-named
// statistics object (NamePrefix) in the table's own schema, of the kinds
// the statistics verifier understands (ndistinct, dependencies, mcv), on
// 2 to 8 plain columns of one schema-qualified table; undone by DROP
// STATISTICS IF EXISTS of exactly that object. Every other form is refused,
// so pg_sage never creates or drops a statistics object it cannot name as
// its own. Identifiers are plain or double-quoted without spaces, dots or
// quotes; anything stranger is refused rather than interpreted.
package extstats

import (
	"fmt"
	"regexp"
	"strings"
)

// NamePrefix marks the statistics objects pg_sage creates.
const NamePrefix = "sage_stx_"

// maxIdentBytes is PostgreSQL's identifier limit (NAMEDATALEN - 1); a
// longer name would be silently truncated.
const maxIdentBytes = 63

const minColumns, maxColumns = 2, 8

// supportedKinds are the statistics kinds the verifier judges.
var supportedKinds = map[string]bool{"ndistinct": true, "dependencies": true, "mcv": true}

const ident = `(?:"[A-Za-z0-9_$]+"|[A-Za-z_][A-Za-z0-9_$]*)`

var (
	identPattern     = regexp.MustCompile(`^` + ident + `$`)
	qualifiedPattern = regexp.MustCompile(`^(` + ident + `)\.(` + ident + `) ?;?$`)
	bareNamePattern  = regexp.MustCompile(`^` + ident + ` ?;?$`)
	// headPattern is what lies between CREATE STATISTICS and FROM:
	// [IF NOT EXISTS] name [(kinds)] ON columns.
	headPattern = regexp.MustCompile(`(?i)^(IF NOT EXISTS )?(` + ident + `)(?:\.(` + ident +
		`))?(?: ?\(([^()]*)\) ?| )ON (.+)$`)
	fromPattern = regexp.MustCompile(`(?i) FROM `)
)

// Ident is one identifier as written and as PostgreSQL resolves it.
type Ident struct {
	Raw  string // as written, quotes included
	Name string // canonical: unquoted folded to lower case, quoted as is
}

func newIdent(raw string) Ident {
	if strings.HasPrefix(raw, `"`) {
		return Ident{Raw: raw, Name: strings.Trim(raw, `"`)}
	}
	return Ident{Raw: raw, Name: strings.ToLower(raw)}
}

// Create is a parsed pg_sage CREATE STATISTICS.
type Create struct {
	Schema, Name       Ident
	TableSchema, Table Ident
	Kinds              []string // canonical, in written order; empty = all kinds
	Columns            []string // canonical, in written order
	IfNotExists        bool
}

// QualifiedName is the statistics object as written.
func (c Create) QualifiedName() string { return c.Schema.Raw + "." + c.Name.Raw }

// QualifiedTable is the table as written.
func (c Create) QualifiedTable() string { return c.TableSchema.Raw + "." + c.Table.Raw }

// Rollback is the inverse: the drop of exactly this object.
func (c Create) Rollback() string { return "DROP STATISTICS IF EXISTS " + c.QualifiedName() }

// Analyze is the ANALYZE that builds the new statistics.
func (c Create) Analyze() string { return "ANALYZE " + c.QualifiedTable() }

// Drop is a parsed DROP STATISTICS of one pg_sage statistics object.
type Drop struct {
	Schema, Name Ident
	IfExists     bool
}

// QualifiedName is the dropped object as written.
func (d Drop) QualifiedName() string { return d.Schema.Raw + "." + d.Name.Raw }

// collapse folds every whitespace run to one space.
func collapse(sql string) string { return strings.Join(strings.Fields(sql), " ") }

func hasPrefixFold(text, prefix string) bool {
	return len(text) >= len(prefix) && strings.EqualFold(text[:len(prefix)], prefix)
}

// IsCreate reports a CREATE STATISTICS statement of any form.
func IsCreate(sql string) bool { return hasPrefixFold(collapse(sql), "CREATE STATISTICS ") }

// IsDrop reports a DROP STATISTICS statement of any form.
func IsDrop(sql string) bool { return hasPrefixFold(collapse(sql), "DROP STATISTICS ") }

// OwnName reports a canonical statistics name pg_sage created: the prefix
// followed by at least one byte, within PostgreSQL's identifier limit.
func OwnName(name string) bool {
	return len(name) > len(NamePrefix) && len(name) <= maxIdentBytes &&
		strings.HasPrefix(name, NamePrefix)
}

// checkName refuses a statistics name pg_sage cannot call its own.
func checkName(name Ident) error {
	if len(name.Name) > maxIdentBytes {
		return fmt.Errorf("statistics name %s is longer than %d bytes", name.Raw, maxIdentBytes)
	}
	if !OwnName(name.Name) {
		return fmt.Errorf("statistics name %s must start with %s", name.Raw, NamePrefix)
	}
	return nil
}

// ParseCreate parses the pg_sage CREATE STATISTICS form.
func ParseCreate(sql string) (Create, error) {
	text := collapse(sql)
	if !hasPrefixFold(text, "CREATE STATISTICS ") {
		return Create{}, fmt.Errorf("not a CREATE STATISTICS statement")
	}
	body := text[len("CREATE STATISTICS "):]
	froms := fromPattern.FindAllStringIndex(body, -1)
	if len(froms) == 0 {
		return Create{}, fmt.Errorf("CREATE STATISTICS must read one table (FROM schema.table)")
	}
	last := froms[len(froms)-1]
	var c Create
	if err := c.parseTable(body[last[1]:]); err != nil {
		return Create{}, err
	}
	if err := c.parseHead(body[:last[0]]); err != nil {
		return Create{}, err
	}
	if err := checkName(c.Name); err != nil {
		return Create{}, err
	}
	if c.Schema.Name != c.TableSchema.Name {
		return Create{}, fmt.Errorf("statistics schema %s differs from the table's schema %s",
			c.Schema.Raw, c.TableSchema.Raw)
	}
	return c, nil
}

func (c *Create) parseTable(tail string) error {
	m := qualifiedPattern.FindStringSubmatch(tail)
	if m == nil {
		if bareNamePattern.MatchString(tail) {
			return fmt.Errorf("the statistics table must be schema-qualified")
		}
		return fmt.Errorf("CREATE STATISTICS must read exactly one table, got %q", tail)
	}
	c.TableSchema, c.Table = newIdent(m[1]), newIdent(m[2])
	return nil
}

func (c *Create) parseHead(head string) error {
	idx := headPattern.FindStringSubmatchIndex(head)
	if idx == nil {
		return fmt.Errorf("CREATE STATISTICS must be [IF NOT EXISTS] schema.name " +
			"[(kinds)] ON plain column list")
	}
	group := func(n int) string {
		if idx[2*n] < 0 {
			return ""
		}
		return head[idx[2*n]:idx[2*n+1]]
	}
	c.IfNotExists = group(1) != ""
	if group(3) == "" {
		return fmt.Errorf("the statistics name must be schema-qualified")
	}
	c.Schema, c.Name = newIdent(group(2)), newIdent(group(3))
	if idx[8] >= 0 { // a kind list is written
		kinds, err := parseKinds(group(4))
		if err != nil {
			return err
		}
		c.Kinds = kinds
	}
	columns := group(5)
	if strings.Contains(columns, "(") {
		return fmt.Errorf("statistics may cover plain columns only, not expressions")
	}
	var err error
	c.Columns, err = parseColumns(columns)
	return err
}

// parseKinds reads a written kind list; an empty one is refused.
func parseKinds(list string) ([]string, error) {
	var kinds []string
	seen := map[string]bool{}
	for _, part := range strings.Split(list, ",") {
		kind := strings.ToLower(strings.TrimSpace(part))
		if !supportedKinds[kind] {
			return nil, fmt.Errorf("statistics kind %q is not supported "+
				"(ndistinct, dependencies, mcv)", kind)
		}
		if seen[kind] {
			return nil, fmt.Errorf("statistics kind %q is named twice", kind)
		}
		seen[kind] = true
		kinds = append(kinds, kind)
	}
	return kinds, nil
}

func parseColumns(list string) ([]string, error) {
	parts := strings.Split(list, ",")
	if len(parts) < minColumns || len(parts) > maxColumns {
		return nil, fmt.Errorf("statistics need %d to %d columns, got %d",
			minColumns, maxColumns, len(parts))
	}
	columns := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		raw := strings.TrimSpace(part)
		if !identPattern.MatchString(raw) {
			return nil, fmt.Errorf("%q is not a plain column name", raw)
		}
		name := newIdent(raw).Name
		if len(name) > maxIdentBytes {
			return nil, fmt.Errorf("column %s is longer than %d bytes", raw, maxIdentBytes)
		}
		if seen[name] {
			return nil, fmt.Errorf("column %s is named twice", raw)
		}
		seen[name] = true
		columns = append(columns, name)
	}
	return columns, nil
}

// ParseDrop parses a DROP STATISTICS of one pg_sage statistics object
// (never CASCADE or RESTRICT, never a list).
func ParseDrop(sql string) (Drop, error) {
	text := collapse(sql)
	if !hasPrefixFold(text, "DROP STATISTICS ") {
		return Drop{}, fmt.Errorf("not a DROP STATISTICS statement")
	}
	rest := text[len("DROP STATISTICS "):]
	var d Drop
	if hasPrefixFold(rest, "IF EXISTS ") {
		d.IfExists, rest = true, rest[len("IF EXISTS "):]
	}
	m := qualifiedPattern.FindStringSubmatch(rest)
	if m == nil {
		if bareNamePattern.MatchString(rest) {
			return Drop{}, fmt.Errorf("the dropped statistics must be schema-qualified")
		}
		return Drop{}, fmt.Errorf("DROP STATISTICS must name one statistics object "+
			"without CASCADE or RESTRICT, got %q", rest)
	}
	d.Schema, d.Name = newIdent(m[1]), newIdent(m[2])
	if err := checkName(d.Name); err != nil {
		return Drop{}, err
	}
	return d, nil
}

// Undoes reports whether dropSQL drops exactly the object c creates.
func Undoes(c Create, dropSQL string) bool {
	d, err := ParseDrop(dropSQL)
	return err == nil && d.Schema.Name == c.Schema.Name && d.Name.Name == c.Name.Name
}
