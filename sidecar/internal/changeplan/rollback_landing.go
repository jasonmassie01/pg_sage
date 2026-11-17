package changeplan

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/executor"
)

// A revert's landing, read back from the catalog before a step is recorded
// reverted (plan-02c): the index a CREATE INDEX step built is gone, the
// index a DROP INDEX or REINDEX step changed is there and valid, a setting
// is back at the value the action recorded before it ran. A rollback this
// read cannot check is not landed (fail closed): a person looks at it.

// revertLanding is what the read-back found for one action.
type revertLanding struct {
	outcome string // the action's outcome (rolled_back once its rollback ran)
	pending bool   // a rollback is in progress (rolling_back)
	landed  bool
	note    string
}

// Bounds of a setting's read-back: a reload reaches a pooled session only
// after it has run a statement, so the read is repeated a few times.
const (
	settingReadTries = 10
	settingReadGap   = 300 * time.Millisecond
)

var (
	reDropIndex = regexp.MustCompile(`(?is)^\s*DROP\s+INDEX\s+(?:CONCURRENTLY\s+)?` +
		`(?:IF\s+EXISTS\s+)?(` + identPart + `(?:\.` + identPart + `)?)\s*;?\s*$`)
	reReindex = regexp.MustCompile(`(?is)^\s*REINDEX\s+INDEX\s+(?:CONCURRENTLY\s+)?(` +
		identPart + `(?:\.` + identPart + `)?)\s*;?\s*$`)
	reBareIdent     = regexp.MustCompile(`^[a-z_][a-z0-9_$]*$`)
	reSystemSetting = regexp.MustCompile(`(?is)^\s*ALTER\s+SYSTEM\s+(?:SET|RESET)\s+(` +
		identPart + `(?:\.` + identPart + `)?)`)
)

// readLanding reads action actionLogID's outcome and, once it is rolled
// back, whether its rollback landed.
func (e *Engine) readLanding(ctx context.Context, actionLogID int64) (revertLanding, error) {
	var l revertLanding
	var forwardSQL, rollbackSQL string
	var prior *string
	err := e.inTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `/* pg_sage */ SELECT COALESCE(outcome, ''),
			COALESCE(sql_executed, ''), COALESCE(rollback_sql, ''),
			before_state #>> '{config_change,prior,setting}'
			FROM sage.action_log WHERE id = $1`, actionLogID).Scan(&l.outcome, &forwardSQL,
			&rollbackSQL, &prior)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return l, fmt.Errorf("action %d: %w", actionLogID, ErrNotFound)
	}
	if err != nil {
		return l, fmt.Errorf("read action %d: %w", actionLogID, err)
	}
	switch l.outcome {
	case "rolling_back":
		l.pending, l.note = true, "a rollback is in progress"
		return l, nil
	case "rolled_back":
	default:
		l.note = fmt.Sprintf("action %d's outcome is %q, not rolled_back", actionLogID,
			l.outcome)
		return l, nil
	}
	if why := rollbackMismatch(forwardSQL, rollbackSQL); why != "" {
		l.note = why
		return l, nil
	}
	l.landed, l.note, err = e.readBack(ctx, rollbackSQL, prior)
	return l, err
}

// rollbackMismatch is why rollbackSQL cannot undo forwardSQL: the two name
// different indexes or settings, so reading the rollback's own target back
// would not show the pre-step state. Empty when they name the same object,
// or when forwardSQL names none this check knows (an index replace, which
// RollbackIndexReplace undoes from its own record).
func rollbackMismatch(forwardSQL, rollbackSQL string) string {
	if executor.IsIndexReplaceSQL(forwardSQL) {
		return ""
	}
	want := touchedObject(forwardSQL)
	if want == "" {
		return ""
	}
	if got := touchedObject(rollbackSQL); got != want {
		return fmt.Sprintf("the rollback %q does not undo the action's %s: a person "+
			"checks it", truncate(rollbackSQL, 200), want)
	}
	return ""
}

// touchedObject is the one index or setting sql creates, drops, rebuilds
// or sets ("index schema.name", "setting name"), empty for anything else.
func touchedObject(sql string) string {
	if m := reDropIndex.FindStringSubmatch(sql); m != nil {
		return "index " + plainIdent(qualifyIndex(m[1]))
	}
	if m := reReindex.FindStringSubmatch(sql); m != nil {
		return "index " + plainIdent(qualifyIndex(m[1]))
	}
	if index, _, ok := newIndexOf(Step{Kind: KindAction, ProposedSQL: sql}); ok &&
		index != "" {
		return "index " + plainIdent(index)
	}
	if m := reSystemSetting.FindStringSubmatch(sql); m != nil {
		return "setting " + strings.ToLower(m[1])
	}
	return ""
}

// plainIdent drops the quotes around each part of a qualified name that
// needs none, so "public"."idx" and public.idx compare equal.
func plainIdent(name string) string {
	parts := splitIdent(name)
	for i, p := range parts {
		if inner := strings.Trim(p, `"`); len(p) > 2 && reBareIdent.MatchString(inner) {
			parts[i] = inner
		}
	}
	return strings.Join(parts, ".")
}

// readBack checks the catalog for what rollbackSQL was to restore.
func (e *Engine) readBack(ctx context.Context, rollbackSQL string, prior *string) (bool,
	string, error) {
	if m := reDropIndex.FindStringSubmatch(rollbackSQL); m != nil {
		name := qualifyIndex(m[1])
		present, _, err := e.readIndex(ctx, name)
		if present {
			return false, "index " + name + " is still there", err
		}
		return err == nil, "index " + name + " is gone", err
	}
	name := ""
	if m := reReindex.FindStringSubmatch(rollbackSQL); m != nil {
		name = qualifyIndex(m[1])
	} else if index, _, ok := newIndexOf(Step{Kind: KindAction,
		ProposedSQL: rollbackSQL}); ok && index != "" {
		name = index
	}
	if name != "" {
		present, valid, err := e.readIndex(ctx, name)
		if present && valid {
			return true, "index " + name + " is there and valid", err
		}
		return false, fmt.Sprintf("index %s is not there and valid (present %v)", name,
			present), err
	}
	if m := reSystemSetting.FindStringSubmatch(rollbackSQL); m != nil {
		return e.readSetting(ctx, strings.ToLower(m[1]), prior)
	}
	return false, fmt.Sprintf("no read-back for the rollback %q: a person checks it",
		truncate(rollbackSQL, 200)), nil
}

// qualifyIndex is an unqualified index name in public, as a rollback's
// search path resolves it, and a qualified one as it is.
func qualifyIndex(name string) string { return qualify(name, "public") }

// readIndex reports whether index name exists and is valid.
func (e *Engine) readIndex(ctx context.Context, name string) (present, valid bool,
	err error) {
	err = e.inTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `/* pg_sage */ SELECT to_regclass($1) IS NOT NULL,
			COALESCE((SELECT indisvalid FROM pg_catalog.pg_index
			           WHERE indexrelid = to_regclass($1)), false)`, name).Scan(&present,
			&valid)
	})
	if err != nil {
		return false, false, fmt.Errorf("read index %s back: %w", name, err)
	}
	return present, valid, nil
}

// readSetting reports whether setting name is back at prior, the value
// the action recorded before it ran, and in effect (no restart pending).
func (e *Engine) readSetting(ctx context.Context, name string, prior *string) (bool, string,
	error) {
	if prior == nil {
		return false, "the action recorded no prior value of " + name + " to read back", nil
	}
	var got string
	var pendingRestart bool
	for try := range settingReadTries {
		err := e.inTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `/* pg_sage */ SELECT setting, pending_restart
				FROM pg_catalog.pg_settings WHERE name = $1`, name).Scan(&got, &pendingRestart)
		})
		if err != nil {
			return false, "", fmt.Errorf("read setting %s back: %w", name, err)
		}
		if got == *prior && !pendingRestart {
			return true, fmt.Sprintf("%s is back at %q", name, got), nil
		}
		if try < settingReadTries-1 {
			select {
			case <-ctx.Done():
				return false, "", ctx.Err()
			case <-time.After(settingReadGap):
			}
		}
	}
	return false, fmt.Sprintf("%s is %q (pending restart %v), not its prior %q", name, got,
		pendingRestart, *prior), nil
}
