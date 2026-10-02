package pooler

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// pgxAdmin is a PgBouncer admin-console session over pgx.
type pgxAdmin struct{ conn *pgx.Conn }

// errDSN never carries the DSN it could not parse.
var errDSN = errors.New("the pooler DSN does not parse")

// DialPgBouncer opens a PgBouncer admin-console session. The console
// speaks only the simple query protocol, so statements are neither
// prepared nor cached.
func DialPgBouncer(ctx context.Context, dsn string) (Admin, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errDSN
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.StatementCacheCapacity, cfg.DescriptionCacheCapacity = 0, 0
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &pgxAdmin{conn: conn}, nil
}

// Show runs SHOW POOLS or SHOW STATS, reading at most the probe row
// ceiling plus one (so the cut is detectable).
func (a *pgxAdmin) Show(ctx context.Context, what string) ([]map[string]any, error) {
	var sql string
	switch what {
	case ShowPools:
		sql = "SHOW POOLS"
	case ShowStats:
		sql = "SHOW STATS"
	default:
		return nil, fmt.Errorf("SHOW %q is not a pooler telemetry command", what)
	}
	rows, err := a.conn.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	fields := rows.FieldDescriptions()
	for rows.Next() && len(out) <= probes.MaxRows {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		row := make(map[string]any, len(fields))
		for i, f := range fields {
			if i < len(vals) {
				row[f.Name] = vals[i]
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Close ends the session.
func (a *pgxAdmin) Close(ctx context.Context) error { return a.conn.Close(ctx) }
