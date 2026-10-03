package executor

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

var (
	// ErrConfigPrior: the prior value could not be read, so no faithful
	// rollback exists and the change is refused.
	ErrConfigPrior = errors.New("cannot capture the prior configuration value")
	// ErrConfigOverridden: a command-line, database, role or session
	// setting shadows ALTER SYSTEM, so the change could not take effect here
	// and the server-level prior cannot be read.
	ErrConfigOverridden = errors.New("setting is overridden above ALTER SYSTEM")
	// ErrConfigPendingChange: the setting already has an unapplied change
	// waiting for a restart; its file value is ambiguous.
	ErrConfigPendingChange = errors.New("setting already has a change pending restart")
	// ErrNoFaithfulRollback: no single statement restores every prior value.
	ErrNoFaithfulRollback = errors.New("no single-statement rollback restores the prior state")
)

// gucRollbackSQL builds the statement that restores a setting's prior
// server-level state. RESET is used only when the prior did not come from
// postgresql.auto.conf (built-in default, computed, environment, or a
// postgresql.conf line that ALTER SYSTEM merely shadows); otherwise the
// prior effective value is set back.
func gucRollbackSQL(name string, prior settingRow) (string, error) {
	if prior.PendingRestart {
		return "", fmt.Errorf("%w: %s", ErrConfigPendingChange, name)
	}
	reset := "ALTER SYSTEM RESET " + name
	switch prior.Source {
	case "default", "override", "environment variable":
		return reset, nil
	case "configuration file":
		if prior.SourceFile != "" && !isAutoConf(prior.SourceFile) {
			return reset, nil
		}
		return "ALTER SYSTEM SET " + name + " = " + sanitize.QuoteLiteral(prior.Setting), nil
	case "command line":
		return "", fmt.Errorf("%w: %s is set on the server command line, which "+
			"ALTER SYSTEM cannot override", ErrConfigOverridden, name)
	}
	return "", fmt.Errorf("%w: %s comes from %q (database, role or session level); "+
		"a server-level change would not take effect here", ErrConfigOverridden, name,
		prior.Source)
}

// reloptionRollbackSQL builds the statement restoring every key the
// change touches: RESET when none was set before, SET of the prior values
// when all were. A mix needs two subcommands, which the executor never
// runs, so it is refused.
func reloptionRollbackSQL(stmt pgconf.TableStmt, main, toast map[string]string) (string, error) {
	var setParts, resetKeys []string
	for _, opt := range stmt.Options {
		base, isToast := pgconf.ReloptionBaseKey(opt.Key)
		prior := main
		if isToast {
			prior = toast
		}
		if v, ok := prior[base]; ok {
			setParts = append(setParts, opt.Key+" = "+reloptionLiteral(v))
		} else {
			resetKeys = append(resetKeys, opt.Key)
		}
	}
	switch {
	case len(setParts) > 0 && len(resetKeys) > 0:
		return "", fmt.Errorf("%w: %s were set before but %s were not", ErrNoFaithfulRollback,
			strings.Join(setParts, ", "), strings.Join(resetKeys, ", "))
	case len(setParts) > 0:
		return "ALTER TABLE " + stmt.Table + " SET (" + strings.Join(setParts, ", ") + ")", nil
	}
	return "ALTER TABLE " + stmt.Table + " RESET (" + strings.Join(resetKeys, ", ") + ")", nil
}

var bareReloptionValue = regexp.MustCompile(`^[A-Za-z0-9_.+-]+$`)

// reloptionLiteral renders a stored reloption value for SQL.
func reloptionLiteral(v string) string {
	if bareReloptionValue.MatchString(v) {
		return v
	}
	return sanitize.QuoteLiteral(v)
}

// parseReloptionArray turns pg_class.reloptions ("k=v") into a map.
func parseReloptionArray(opts []string) map[string]string {
	out := make(map[string]string, len(opts))
	for _, kv := range opts {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[strings.ToLower(k)] = v
		}
	}
	return out
}

// captureReloptions reads a table's and its TOAST table's reloptions.
func captureReloptions(
	ctx context.Context, pool *pgxpool.Pool, table string,
) (main, toast map[string]string, err error) {
	var mainOpts, toastOpts []string
	err = pool.QueryRow(ctx, `/* pg_sage */ SELECT COALESCE(c.reloptions, '{}'),
		COALESCE(t.reloptions, '{}')
		FROM pg_class c LEFT JOIN pg_class t ON t.oid = c.reltoastrelid
		WHERE c.oid = to_regclass($1)`, table).Scan(&mainOpts, &toastOpts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, fmt.Errorf("%w: table %s does not exist", ErrConfigPrior, table)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%w: reloptions of %s: %v", ErrConfigPrior, table, err)
	}
	return parseReloptionArray(mainOpts), parseReloptionArray(toastOpts), nil
}
