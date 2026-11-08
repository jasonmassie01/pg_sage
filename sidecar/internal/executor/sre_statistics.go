package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/extstats"
)

// StatisticsTarget binds an operator request to one physical table and its
// columns. Names alone are insufficient: a replacement table must be refused.
type StatisticsTarget struct {
	DatabaseOID uint32             `json:"database_oid"`
	TableOID    uint32             `json:"table_oid"`
	OwnerOID    uint32             `json:"owner_oid"`
	Schema      string             `json:"schema"`
	Table       string             `json:"table"`
	Relation    string             `json:"relation"`
	Columns     []StatisticsColumn `json:"columns"`
}
type StatisticsColumn struct {
	Name         string `json:"name"`
	Number       int16  `json:"number"`
	TypeOID      uint32 `json:"type_oid"`
	TypeMod      int32  `json:"type_mod"`
	CollationOID uint32 `json:"collation_oid"`
}

var ErrStatisticsTargetInvalid = errors.New("statistics target unavailable in permitted catalog scope")

type statisticsQuery interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// CaptureStatisticsTarget is a bounded catalog read; it never reads table data.
func CaptureStatisticsTarget(ctx context.Context, pool *pgxpool.Pool, schema, table string, columns []string) (StatisticsTarget, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	t, err := captureStatisticsTarget(ctx, pool, schema, table, columns)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrStatisticsTargetInvalid
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42501" {
		return t, ErrStatisticsTargetInvalid
	}
	return t, err
}
func captureStatisticsTarget(ctx context.Context, q statisticsQuery, schema, table string, columns []string) (StatisticsTarget, error) {
	t := StatisticsTarget{Schema: schema, Table: table}
	if len(columns) < 2 || len(columns) > 8 {
		return t, ErrStatisticsTargetInvalid
	}
	err := q.QueryRow(ctx, `/* pg_sage */ SELECT d.oid,c.oid,c.relowner,quote_ident(n.nspname)||'.'||quote_ident(c.relname)
 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 JOIN pg_catalog.pg_database d ON d.datname=current_database()
 WHERE n.nspname=$1 AND c.relname=$2 AND c.relkind='r' AND n.nspname NOT IN ('sage','pg_catalog','information_schema')
 AND n.nspname NOT LIKE 'pg_temp_%'`, schema, table).Scan(&t.DatabaseOID, &t.TableOID, &t.OwnerOID, &t.Relation)
	if err != nil {
		return t, fmt.Errorf("capture statistics table: %w", err)
	}
	seen := map[string]bool{}
	for _, name := range columns {
		if seen[name] {
			return t, ErrStatisticsTargetInvalid
		}
		seen[name] = true
		c := StatisticsColumn{Name: name}
		err = q.QueryRow(ctx, `SELECT attnum,atttypid,atttypmod,attcollation FROM pg_catalog.pg_attribute
   WHERE attrelid=$1 AND attname=$2 AND attnum>0 AND NOT attisdropped`, t.TableOID, name).Scan(&c.Number, &c.TypeOID, &c.TypeMod, &c.CollationOID)
		if err != nil {
			return t, fmt.Errorf("capture statistics column: %w", err)
		}
		t.Columns = append(t.Columns, c)
	}
	return t, nil
}

type sreStatisticsKey struct{}
type sreStatisticsRun struct {
	Target    StatisticsTarget
	SQL       string
	ObjectOID uint32
	Rollback  bool
	Authorize func(context.Context) error
}

// PreviewStatistics preserves the existing tuning/statistics standing-policy gate.
func (e *Executor) PreviewStatistics(ctx context.Context, sql string) ActionPolicyDecision {
	return e.explainOperatorAction(ctx, sql)
}

// ExecuteSREStatistics is the typed bridge's sole entry to the manual executor.
// Human approval and a fresh durable queue claim are validated by its owner.
func (e *Executor) ExecuteSREStatistics(ctx context.Context, findingID int, sql string, approvedBy int, target StatisticsTarget) (int64, uint32, error) {
	if approvedBy <= 0 {
		return 0, 0, ErrBackendApprovalRequired
	}
	c, err := extstats.ParseCreate(sql)
	if err != nil {
		return 0, 0, err
	}
	if c.TableSchema.Name != target.Schema || c.Table.Name != target.Table {
		return 0, 0, errors.New("statistics target mismatch")
	}
	r := &sreStatisticsRun{Target: target, SQL: sql}
	ctx = context.WithValue(ctx, sreStatisticsKey{}, r)
	id, err := e.ExecuteManual(ctx, findingID, sql, c.Rollback(), &approvedBy)
	return id, r.ObjectOID, err
}

// RollbackSREStatistics removes exactly the created object, after an OID check
// under the same table lock as DROP. It cannot restore pre-ANALYZE statistics.
func (e *Executor) RollbackSREStatistics(ctx context.Context, actionID int64, sql string, target StatisticsTarget, objectOID uint32, reason string, approvedBy int) error {
	if approvedBy <= 0 {
		return ErrBackendApprovalRequired
	}
	if objectOID == 0 {
		return errors.New("missing created statistics identity")
	}
	c, err := extstats.ParseCreate(sql)
	if err != nil {
		return err
	}
	var stored string
	if err = e.pool.QueryRow(ctx, `SELECT COALESCE(rollback_sql,'') FROM sage.action_log WHERE id=$1`, actionID).Scan(&stored); err != nil {
		return err
	}
	if stored != c.Rollback() {
		return errors.New("statistics rollback does not match created object")
	}
	r := &sreStatisticsRun{Target: target, SQL: sql, ObjectOID: objectOID, Rollback: true}
	r.Authorize = func(ctx context.Context) error {
		if err := e.manualMutationBlock(ctx); err != nil {
			return err
		}
		_, err := e.authorizeOperatorAction(ctx, c.Rollback(), 0, &approvedBy)
		return err
	}
	if err = r.Authorize(ctx); err != nil {
		return err
	}
	return e.RollbackAction(context.WithValue(ctx, sreStatisticsKey{}, r), actionID, reason+"; ANALYZE statistics are not restored")
}

func statisticsRun(ctx context.Context) *sreStatisticsRun {
	r, _ := ctx.Value(sreStatisticsKey{}).(*sreStatisticsRun)
	return r
}
func (r *sreStatisticsRun) guard(ctx context.Context, tx pgx.Tx) error {
	c, err := extstats.ParseCreate(r.SQL)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "LOCK TABLE "+c.QualifiedTable()+" IN SHARE UPDATE EXCLUSIVE MODE"); err != nil {
		return wrapDDLError(err)
	}
	names := make([]string, len(r.Target.Columns))
	for i, v := range r.Target.Columns {
		names[i] = v.Name
	}
	fresh, err := captureStatisticsTarget(ctx, tx, r.Target.Schema, r.Target.Table, names)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(fresh, r.Target) {
		return errors.New("statistics table or column identity changed")
	}
	var oid uint32
	err = tx.QueryRow(ctx, `SELECT s.oid FROM pg_catalog.pg_statistic_ext s JOIN pg_catalog.pg_namespace n ON n.oid=s.stxnamespace WHERE n.nspname=$1 AND s.stxname=$2`, c.Schema.Name, c.Name.Name).Scan(&oid)
	if r.Rollback {
		if err != nil || oid != r.ObjectOID {
			return errors.New("created statistics object changed or disappeared")
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		if err != nil {
			return err
		}
		return errors.New("statistics object already exists")
	}
	if r.Authorize != nil {
		return r.Authorize(ctx)
	}
	return nil
}
func (r *sreStatisticsRun) verify(ctx context.Context, tx pgx.Tx) error {
	c, _ := extstats.ParseCreate(r.SQL)
	var built bool
	err := tx.QueryRow(ctx, `SELECT s.oid, EXISTS(SELECT 1 FROM pg_catalog.pg_stats_ext v WHERE v.statistics_schemaname=n.nspname AND v.statistics_name=s.stxname AND (v.n_distinct IS NOT NULL OR v.dependencies IS NOT NULL OR v.most_common_vals IS NOT NULL))
 FROM pg_catalog.pg_statistic_ext s JOIN pg_catalog.pg_namespace n ON n.oid=s.stxnamespace
 WHERE n.nspname=$1 AND s.stxname=$2 AND s.stxrelid=$3 AND s.stxowner=(SELECT oid FROM pg_catalog.pg_roles WHERE rolname=current_user)`, c.Schema.Name, c.Name.Name, r.Target.TableOID).Scan(&r.ObjectOID, &built)
	if err != nil {
		return err
	}
	if !built {
		return errors.New("created statistics are not built or not visible")
	}
	return nil
}

func validateSREStatisticsFinding(ctx context.Context, category string, detail json.RawMessage, sql string) error {
	if category != "sre_maintenance" {
		return nil
	}
	r := statisticsRun(ctx)
	if r == nil || r.Rollback || strings.TrimSpace(r.SQL) != strings.TrimSpace(sql) {
		return errors.New("SRE maintenance requires the typed approved runner")
	}
	var p struct {
		Target StatisticsTarget `json:"target"`
		SQL    string           `json:"sql"`
	}
	if err := json.Unmarshal(detail, &p); err != nil {
		return err
	}
	if p.SQL != sql || !reflect.DeepEqual(p.Target, r.Target) {
		return errors.New("SRE maintenance finding identity mismatch")
	}
	return nil
}

// StatisticsPoolMatches binds maintenance discovery and execution to one pool.
func (e *Executor) StatisticsPoolMatches(pool *pgxpool.Pool) bool { return e != nil && e.pool == pool }
