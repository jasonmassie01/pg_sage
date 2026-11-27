package shadow

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/pg-sage/sidecar/internal/optimizer"
)

// A change found in the catalog is applied outside pg_sage only when no
// action of pg_sage's own explains it. The pass's fact window (factBatch
// actions from its oldest pending decision) can end before pg_sage's own
// index change on a busy database, so the scorer asks the action ledger
// directly, once the catalog says the change is there: pg_sage's index
// changes since the decision that name its table (a create) or index (a
// drop), compared by shape. A change pg_sage made is no external
// evidence; the decision is left to the horizon (unscored).

// ownActionLimit bounds the own index actions one lookup reads. Past it
// the lookup cannot rule pg_sage out, and it fails closed.
const ownActionLimit = 50

// ownIndexActionsSQL walks idx_action_log_index_ddl (schema.selfexclIndexes).
const ownIndexActionsSQL = `/* pg_sage */ SELECT l.action_type, l.sql_executed
	FROM sage.action_log l
	WHERE l.action_type IN ('create_index', 'drop_index', 'replace_index')
	  AND l.executed_at >= $1 AND l.outcome <> 'failed'
	  AND l.sql_executed ILIKE '%' || $2::text || '%'
	ORDER BY l.executed_at, l.id LIMIT $3`

// madeByPgSage reports whether one of pg_sage's own index actions since
// d was recorded made d's change (or may have: true when it cannot tell).
func (s *Scorer) madeByPgSage(ctx context.Context, d Decision) (bool, error) {
	needle, ok := ownActionNeedle(d)
	if !ok {
		return true, nil
	}
	rows, err := s.pool.Query(ctx, ownIndexActionsSQL, d.RecordedAt, needle,
		ownActionLimit+1)
	if err != nil {
		return false, fmt.Errorf("shadow: read own index actions for decision %d: %w",
			d.ID, err)
	}
	defer rows.Close()
	read := 0
	for rows.Next() {
		var actionType, sql string
		if err := rows.Scan(&actionType, &sql); err != nil {
			return false, fmt.Errorf("shadow: scan own index action for decision %d: %w",
				d.ID, err)
		}
		read++
		if slices.Contains(actionShapes(actionType, sql), d.Shape) {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("shadow: read own index actions for decision %d: %w",
			d.ID, err)
	}
	return read > ownActionLimit, nil
}

// likeEscaper escapes a literal for ILIKE (default escape character).
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// ownActionNeedle is the ILIKE literal every action that made d's change
// contains: the table of an index create, the index of a drop.
func ownActionNeedle(d Decision) (string, bool) {
	var qualified string
	ok := false
	switch d.Class {
	case "index_create":
		qualified, ok = shapeTable(d.Shape)
	case "index_drop":
		qualified, ok = strings.CutPrefix(d.Shape, "drop index ")
	}
	name := lastIdentifier(qualified)
	if !ok || name == "" {
		return "", false
	}
	return likeEscaper.Replace(name), true
}

// lastIdentifier is the unqualified name of a shape's [schema.]name,
// without the quotes of a quoted one; its doubled quotes stay doubled, as
// in SQL text.
func lastIdentifier(qualified string) string {
	if len(qualified) < 2 || !strings.HasSuffix(qualified, `"`) {
		return qualified[strings.LastIndex(qualified, ".")+1:]
	}
	for i := len(qualified) - 2; i >= 0; i-- {
		if qualified[i] != '"' {
			continue
		}
		if i > 0 && qualified[i-1] == '"' {
			i--
			continue
		}
		return qualified[i+1 : len(qualified)-1]
	}
	return ""
}

// actionShapes are the shapes of an action's statements: both of an index
// replacement, else the one.
func actionShapes(actionType, sql string) []string {
	if actionType == "replace_index" {
		if create, drop, ok := optimizer.SplitIndexReplaceSQL(sql); ok {
			return []string{Shape(create), Shape(drop)}
		}
	}
	return []string{Shape(sql)}
}
