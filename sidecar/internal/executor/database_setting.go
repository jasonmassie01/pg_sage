package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

type databaseSettingPrior struct {
	Database string  `json:"database"`
	Value    *string `json:"value"`
}

func databaseName(stmt pgconf.DatabaseStmt) string {
	if strings.HasPrefix(stmt.Database, `"`) {
		return unquoteIdentifier(stmt.Database)
	}
	return strings.ToLower(stmt.Database)
}

// Capture only the database-wide entry (setrole=0), never a role/session
// override that would be widened if restored as a database-wide setting.
func captureDatabaseSetting(ctx context.Context, pool *pgxpool.Pool, stmt pgconf.DatabaseStmt) (databaseSettingPrior, error) {
	prior := databaseSettingPrior{Database: databaseName(stmt)}
	var exists bool
	err := pool.QueryRow(ctx, `/* pg_sage */ SELECT EXISTS(SELECT FROM pg_database WHERE datname=$1),
		(SELECT substring(setting from length($2)+2) FROM pg_db_role_setting s
		JOIN pg_database d ON d.oid=s.setdatabase CROSS JOIN LATERAL unnest(s.setconfig) setting
		WHERE d.datname=$1 AND s.setrole=0 AND split_part(setting,'=',1)=$2)`, prior.Database, stmt.Name).Scan(&exists, &prior.Value)
	if err != nil || !exists {
		return prior, fmt.Errorf("%w: database setting %s/%s: exists=%v error=%v", ErrConfigPrior, prior.Database, stmt.Name, exists, err)
	}
	return prior, nil
}

func databaseRollbackSQL(stmt pgconf.DatabaseStmt, prior databaseSettingPrior) string {
	prefix := "ALTER DATABASE " + pgx.Identifier{prior.Database}.Sanitize()
	if prior.Value == nil {
		return prefix + " RESET " + stmt.Name
	}
	return prefix + " SET " + stmt.Name + " = " + sanitize.QuoteLiteral(*prior.Value)
}

// A separate pool guarantees a fresh physical session. The config retains the
// production BeforeConnect OAuth hook and verified TLS; no retained session
// can masquerade as fresh read-back after ALTER DATABASE.
func freshDatabaseSetting(ctx context.Context, pool *pgxpool.Pool, stmt pgconf.DatabaseStmt) (settingRow, error) {
	cfg := pool.Config()
	cfg.ConnConfig.Database = databaseName(stmt)
	cfg.MinConns, cfg.MaxConns = 0, 1
	fresh, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return settingRow{}, err
	}
	defer fresh.Close()
	return readSettingRow(ctx, fresh, stmt.Name)
}

func readDatabaseSetting(ctx context.Context, pool *pgxpool.Pool, stmt pgconf.DatabaseStmt) (readbackState, string, map[string]any) {
	stored, err := captureDatabaseSetting(ctx, pool, stmt)
	if err != nil {
		return readbackUnconfirmed, "database setting catalog read-back failed: " + err.Error(), map[string]any{"state": readbackUnconfirmed}
	}
	row, err := freshDatabaseSetting(ctx, pool, stmt)
	if err != nil {
		return readbackUnconfirmed, "fresh database session read-back failed: " + err.Error(), map[string]any{"state": readbackUnconfirmed, "stored": stored.Value}
	}
	matched := stmt.Reset && stored.Value == nil && row.Source != "database"
	if !stmt.Reset {
		matched = stored.Value != nil && strings.EqualFold(*stored.Value, stmt.Value) && settingMatches(row, stmt.Value) && row.Source == "database"
	}
	state, note := readbackNotInEffect, "database setting did not take effect in a fresh session"
	if matched {
		state, note = readbackInEffect, "database setting in effect in a fresh physical session"
	}
	return state, note, map[string]any{"state": state, "database": stored.Database, "stored": stored.Value, "effective": row.Setting, "source": row.Source, "fresh_session": true}
}
