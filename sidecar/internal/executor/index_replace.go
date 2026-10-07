package executor

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/optimizer"
)

// The replace action (roadmap 2.3): create the wider index CONCURRENTLY,
// check it is valid, then drop the index it subsumes CONCURRENTLY. The
// drop is soft: the old definition is kept, watched over the drop window
// and re-created on regression. CONCURRENTLY cannot run in a transaction,
// so the action is two steps behind a durable state machine
// (sage.index_replace); it runs only with an operator's approval and holds
// the table's change lease for both steps.

// ActionTypeReplaceIndex is the replace action's contract type and its
// sage.action_log label.
const ActionTypeReplaceIndex = "replace_index"

var (
	// ErrReplaceInvalid: the statement pair or its undo is not a safe
	// replacement of one index by another on the same table.
	ErrReplaceInvalid = errors.New("index replace: invalid statement pair")
	// ErrReplaceConstraintBacked: the old index backs a constraint or
	// enforces uniqueness; it is never replaced.
	ErrReplaceConstraintBacked = errors.New(
		"index replace: the old index backs a constraint or enforces uniqueness")
	// ErrReplaceForeignKey: the new index would not support a foreign key
	// the old index supports.
	ErrReplaceForeignKey = errors.New(
		"index replace: the new index would not support a foreign key the old index supports")
	// ErrReplaceIdentityChanged: the old index is not the one proposed (its
	// OID or definition changed).
	ErrReplaceIdentityChanged = errors.New(
		"index replace: the old index changed since it was proposed")
	// ErrReplaceNotSubsumed: the new index does not serve every lookup of
	// the old one.
	ErrReplaceNotSubsumed = errors.New(
		"index replace: the new index does not subsume the old one")
	// ErrReplaceCreateFailed: the build failed; nothing was dropped.
	ErrReplaceCreateFailed = errors.New(
		"index replace: creating the new index failed; nothing was dropped")
	// ErrReplacePartial: the new index was built but the old one could not
	// be dropped; both stay.
	ErrReplacePartial = errors.New(
		"index replace: partial result: the new index was created but the old one " +
			"could not be dropped")
	// ErrReplaceApprovalRequired: the executor cycle never runs a replace
	// on its own.
	ErrReplaceApprovalRequired = errors.New(
		"index replace runs only with an operator approval")
)

// IndexReplace is a parsed replacement: the forward pair and its undo, with
// the objects they name (schema-qualified).
type IndexReplace struct {
	Table, NewIndex, OldIndex string
	CreateSQL, DropSQL        string
	RecreateSQL, DropNewSQL   string
}

var ifNotExists = regexp.MustCompile(`(?i)^\s*CREATE\s+(UNIQUE\s+)?INDEX\s+` +
	`(CONCURRENTLY\s+)?IF\s+NOT\s+EXISTS\b`)

// IsIndexReplaceSQL reports a replacement's statement pair.
func IsIndexReplaceSQL(sql string) bool {
	_, _, ok := optimizer.SplitIndexReplaceSQL(sql)
	return ok
}

func replaceInvalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrReplaceInvalid, fmt.Sprintf(format, args...))
}

// ParseIndexReplace parses and checks a replacement and its undo: one
// named, non-unique CREATE INDEX CONCURRENTLY on a schema-qualified table,
// the DROP INDEX CONCURRENTLY of another index of that schema, and an undo
// that re-creates exactly that index on that table and drops exactly the
// new one. Every statement passes the executor's SQL validation.
func ParseIndexReplace(sql, rollbackSQL string) (IndexReplace, error) {
	create, drop, ok := optimizer.SplitIndexReplaceSQL(sql)
	if !ok {
		return IndexReplace{}, replaceInvalid("not a CREATE INDEX CONCURRENTLY and " +
			"DROP INDEX CONCURRENTLY pair")
	}
	recreate, dropNew, ok := optimizer.SplitIndexReplaceSQL(rollbackSQL)
	if !ok {
		return IndexReplace{}, replaceInvalid("the undo is not a re-create and drop pair")
	}
	for _, stmt := range []string{create, drop, recreate, dropNew} {
		if err := ValidateExecutorSQL(stmt); err != nil {
			return IndexReplace{}, replaceInvalid("%v", err)
		}
	}
	plan, err := parseReplaceCreate(create)
	if err != nil {
		return IndexReplace{}, err
	}
	plan.CreateSQL, plan.DropSQL, plan.RecreateSQL, plan.DropNewSQL =
		create, drop, recreate, dropNew
	if err := plan.parseDrop(drop); err != nil {
		return IndexReplace{}, err
	}
	return plan, plan.checkUndo(recreate, dropNew)
}

func parseReplaceCreate(create string) (IndexReplace, error) {
	spec, err := optimizer.ParseIndexDDL(create)
	switch {
	case err != nil:
		return IndexReplace{}, replaceInvalid("%v", err)
	case spec.Unique:
		return IndexReplace{}, replaceInvalid("a replacement never adds a uniqueness rule")
	case ifNotExists.MatchString(create):
		return IndexReplace{}, replaceInvalid("IF NOT EXISTS cannot bind the new index")
	case spec.Name == "":
		return IndexReplace{}, replaceInvalid("the new index must be named")
	case spec.TableSchema == "":
		return IndexReplace{}, replaceInvalid("the table must be schema-qualified")
	}
	return IndexReplace{Table: qualifiedName(spec.TableSchema, spec.TableName),
		NewIndex: qualifiedName(spec.TableSchema, spec.Name)}, nil
}

func (p *IndexReplace) parseDrop(drop string) error {
	target, err := supersededIndexTarget(drop)
	if err != nil {
		return replaceInvalid("%v", err)
	}
	parts := identifierParts(target)
	if len(parts) != 2 {
		return replaceInvalid("the old index must be schema-qualified")
	}
	p.OldIndex = qualifiedName(parts[0], parts[1])
	if schemaOf(p.OldIndex) != schemaOf(p.Table) {
		return replaceInvalid("%s is not in the schema of %s", p.OldIndex, p.Table)
	}
	if p.OldIndex == p.NewIndex {
		return replaceInvalid("the pair drops the index it creates")
	}
	return nil
}

func (p IndexReplace) checkUndo(recreate, dropNew string) error {
	spec, err := optimizer.ParseIndexDDL(recreate)
	if err != nil {
		return replaceInvalid("undo: %v", err)
	}
	schema := spec.TableSchema
	if schema == "" {
		schema = schemaOf(p.Table)
	}
	if qualifiedName(schema, spec.Name) != p.OldIndex ||
		qualifiedName(schema, spec.TableName) != p.Table {
		return replaceInvalid("the undo re-creates %s on %s, not %s on %s",
			qualifiedName(schema, spec.Name), qualifiedName(schema, spec.TableName),
			p.OldIndex, p.Table)
	}
	target, err := supersededIndexTarget(dropNew)
	if err != nil {
		return replaceInvalid("undo: %v", err)
	}
	if parts := identifierParts(target); len(parts) != 2 ||
		qualifiedName(parts[0], parts[1]) != p.NewIndex {
		return replaceInvalid("the undo drops %s, not the new index %s", target, p.NewIndex)
	}
	return nil
}

// checkSubsumes reports whether the new index serves every lookup of the
// old index's definition.
func (p IndexReplace) checkSubsumes(oldDefinition string) error {
	if strings.TrimSpace(oldDefinition) == "" ||
		!optimizer.Subsumes(p.CreateSQL, oldDefinition) {
		return fmt.Errorf("%w: %s does not serve every lookup of %s", ErrReplaceNotSubsumed,
			p.NewIndex, p.OldIndex)
	}
	return nil
}

// supportsForeignKey reports whether an index whose leading key columns
// are lead (partial: it has a predicate) supports a foreign key on fk: the
// first len(fk) keys are exactly the foreign key's columns.
func supportsForeignKey(lead []string, partial bool, fk []string) bool {
	if partial || len(fk) == 0 || len(lead) < len(fk) {
		return false
	}
	want := map[string]bool{}
	for _, c := range fk {
		want[c] = true
	}
	for _, c := range lead[:len(fk)] {
		if !want[c] {
			return false
		}
		delete(want, c)
	}
	return len(want) == 0
}

var plainIdentifier = regexp.MustCompile(`^[a-z_][a-z0-9_$]*$`)

// qualifiedName is schema.name with each part quoted only when it must be.
func qualifiedName(schema, name string) string {
	return identifierSQL(schema) + "." + identifierSQL(name)
}

func identifierSQL(name string) string {
	if plainIdentifier.MatchString(name) {
		return name
	}
	return pgx.Identifier{name}.Sanitize()
}

func schemaOf(qualified string) string {
	parts := identifierParts(qualified)
	if len(parts) != 2 {
		return ""
	}
	return parts[0]
}

// identifierParts splits a possibly qualified name as PostgreSQL resolves
// it: quoted parts keep their case, bare parts fold to lower case.
func identifierParts(s string) []string {
	var parts []string
	inQuote, start := false, 0
	add := func(part string) {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, `"`) {
			parts = append(parts, unquoteIdentifier(part))
			return
		}
		parts = append(parts, strings.ToLower(part))
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuote = !inQuote
		case '.':
			if !inQuote {
				add(s[start:i])
				start = i + 1
			}
		}
	}
	add(s[start:])
	return parts
}
