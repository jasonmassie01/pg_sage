package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

const (
	configKindGUC         = "guc"
	configKindDatabaseGUC = "database_guc"
	configKindReloption   = "reloption"
)

// configChange is one ALTER SYSTEM or ALTER TABLE ... SET/RESET (storage
// parameters) prepared for execution (G-P0-1): the prior state captured
// at apply time, the rollback that restores it, and the baseline of the
// metric the change targets.
type configChange struct {
	kind          string
	sql           string
	guc           pgconf.SystemStmt
	table         pgconf.TableStmt
	priorGUC      settingRow
	database      pgconf.DatabaseStmt
	priorDatabase databaseSettingPrior
	priorMain     map[string]string
	priorToast    map[string]string
	rollbackSQL   string
	outcome       *outcomeBaseline
}

// prepareConfigChange captures the prior value of a config change and
// builds its rollback. It returns nil, nil for any other SQL. An error
// means no faithful rollback exists, and the change must not run.
func (e *Executor) prepareConfigChange(ctx context.Context, sql string) (*configChange, error) {
	if ValidateExecutorSQL(sql) != nil {
		return nil, nil // never runs; execution reports the validation error
	}
	c := &configChange{sql: sql}
	if stmt, ok := pgconf.ParseAlterSystem(sql); ok {
		c.kind, c.guc = configKindGUC, stmt
		prior, err := readSettingRow(ctx, e.pool, stmt.Name)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrConfigPrior, err)
		}
		c.priorGUC = prior
		if c.rollbackSQL, err = gucRollbackSQL(stmt.Name, prior); err != nil {
			return nil, err
		}
	} else if stmt, ok := pgconf.ParseAlterDatabase(normalizeSQLText(sql)); ok {
		if e.pool == nil {
			return nil, fmt.Errorf("%w: database pool unavailable", ErrConfigPrior)
		}
		var currentDatabase string
		if err := e.pool.QueryRow(ctx, "/* pg_sage */ SELECT current_database()").Scan(&currentDatabase); err != nil {
			return nil, fmt.Errorf("%w: current database: %v", ErrConfigPrior, err)
		}
		if databaseName(stmt) != currentDatabase {
			return nil, fmt.Errorf("%w: ALTER DATABASE must target the connected database %s", ErrConfigPrior, currentDatabase)
		}
		if err := e.checkLakebaseDatabaseSetting(ctx, sql); err != nil {
			return nil, err
		}
		if !stmt.Reset && strings.EqualFold(stmt.Value, "DEFAULT") {
			return nil, fmt.Errorf("%w: use ALTER DATABASE RESET for a default value", ErrNoFaithfulRollback)
		}
		c.kind, c.database, c.guc = configKindDatabaseGUC, stmt, stmt.SystemStmt
		prior, err := captureDatabaseSetting(ctx, e.pool, stmt)
		if err != nil {
			return nil, err
		}
		fresh, err := freshDatabaseSetting(ctx, e.pool, stmt)
		if err != nil {
			return nil, fmt.Errorf("%w: fresh database session: %v", ErrConfigPrior, err)
		}
		switch fresh.Source {
		case "default", "environment variable", "configuration file", "command line", "global", "database":
		default:
			return nil, fmt.Errorf("%w: database setting %s is masked by %s in a fresh session", ErrConfigOverridden, stmt.Name, fresh.Source)
		}
		c.priorDatabase = prior
		c.rollbackSQL = databaseRollbackSQL(stmt, prior)
	} else if stmt, ok := pgconf.ParseAlterTableReloptions(sql); ok {
		c.kind, c.table = configKindReloption, stmt
		main, toast, err := captureReloptions(ctx, e.pool, stmt.Table)
		if err != nil {
			return nil, err
		}
		c.priorMain, c.priorToast = main, toast
		if c.rollbackSQL, err = reloptionRollbackSQL(stmt, main, toast); err != nil {
			return nil, err
		}
	} else {
		return nil, nil
	}
	if metric, table := outcomeMetricFor(c); metric != "" {
		baseline, err := captureOutcomeBaseline(ctx, e.pool, metric, table)
		if err != nil {
			e.logFn("executor", "outcome baseline for %q unavailable (change will "+
				"be unverifiable): %v", sql, err)
		}
		c.outcome = baseline
	}
	return c, nil
}

// record writes the captured prior state into before_state.
func (c *configChange) record(state map[string]any) {
	if c == nil || state == nil {
		return
	}
	change := map[string]any{"kind": c.kind, "rollback_sql": c.rollbackSQL}
	switch c.kind {
	case configKindGUC:
		change["name"], change["requested"], change["prior"] = c.guc.Name, c.guc.Value,
			c.priorGUC
	case configKindDatabaseGUC:
		change["database"], change["name"], change["requested"], change["prior"] = c.priorDatabase.Database, c.guc.Name, c.guc.Value, c.priorDatabase
	default:
		change["table"] = c.table.Table
		change["prior"] = map[string]any{"table": c.priorMain, "toast": c.priorToast}
	}
	if c.outcome != nil {
		change["outcome"] = c.outcome
	}
	state["config_change"] = change
}

// recordAfterState merges one key into an action's after_state.
func (e *Executor) recordAfterState(ctx context.Context, actionID int64, key string, v any) {
	if err := mergeAfterState(ctx, e.pool, actionID, key, v); err != nil {
		e.logFn("executor", "record %s for action %d: %v", key, actionID, err)
	}
}

func mergeAfterState(
	ctx context.Context, pool *pgxpool.Pool, actionID int64, key string, v any,
) error {
	raw, err := json.Marshal(map[string]any{key: v})
	if err != nil {
		return fmt.Errorf("encode %s: %w", key, err)
	}
	_, err = pool.Exec(ctx, `/* pg_sage */ UPDATE sage.action_log
		SET after_state = COALESCE(after_state, '{}'::jsonb) || $1::jsonb WHERE id = $2`,
		raw, actionID)
	return err
}

// prepareConfig captures a config change's prior state; its rollback
// replaces the one proposed with the approval (G-P0-1).
func (r *manualRun) prepareConfig(ctx context.Context, beforeState map[string]any) error {
	config, err := r.executor.prepareConfigChange(ctx, r.sql)
	if err != nil {
		return fmt.Errorf("config change refused: %w", err)
	}
	if config != nil {
		r.rollbackSQL = config.rollbackSQL
		config.record(beforeState)
	}
	r.config = config
	return nil
}
