package decommission

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Build reads the legacy tables on the control database and returns the
// inventory. It only reads: the legacy tables, pg_namespace and pg_database.
func Build(ctx context.Context, pool *pgxpool.Pool) (Inventory, error) {
	if pool == nil {
		return Inventory{}, errors.New("decommission inventory: no control database pool")
	}
	st, err := load(ctx, pool)
	if err != nil {
		return Inventory{}, fmt.Errorf("decommission inventory: %w", err)
	}
	return buildInventory(st, os.LookupEnv, os.Environ(), time.Now()), nil
}

// queryer is the read side of a pool.
type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func load(ctx context.Context, q queryer) (legacyState, error) {
	st := legacyState{evidence: map[string][]string{}, liveReceipts: map[string]string{},
		receipts: map[string]creationReceipt{}, schemas: map[string]bool{},
		databases: map[string]bool{}, acked: map[string]bool{}}
	present, err := presentTables(ctx, q)
	if err != nil {
		return st, err
	}
	if err := q.QueryRow(ctx, `/* pg_sage agentdb_decommission v1 */
		SELECT current_database()`).Scan(&st.controlDB); err != nil {
		return st, fmt.Errorf("read the control database name: %w", err)
	}
	if present["agentdb_decommission"] {
		if err := loadAcks(ctx, q, st.acked); err != nil {
			return st, err
		}
	}
	st.tablesPresent = present["agent_db_deployments"]
	if !st.tablesPresent {
		return st, nil
	}
	if st.deployments, err = loadDeployments(ctx, q); err != nil {
		return st, err
	}
	if err := loadEvidence(ctx, q, present, &st); err != nil {
		return st, err
	}
	return st, loadLocalArtifacts(ctx, q, &st)
}

// presentTables reports which of the tables the inventory reads exist. An
// install upgraded from an older AgentDB schema may lack later tables.
func presentTables(ctx context.Context, q queryer) (map[string]bool, error) {
	rows, err := q.Query(ctx, `/* pg_sage agentdb_decommission v1 */
		SELECT c.relname FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'sage' AND c.relkind IN ('r', 'p')
		  AND c.relname = ANY($1)`, []string{"agent_db_deployments",
		"agent_db_live_receipts", "agent_db_live_authorizations",
		"agent_db_provision_attempts", "agent_db_creation_receipts",
		"agentdb_decommission"})
	if err != nil {
		return nil, fmt.Errorf("look up the legacy tables: %w", err)
	}
	present := map[string]bool{}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("look up the legacy tables: %w", err)
	}
	for _, name := range names {
		present[name] = true
	}
	return present, nil
}

// deploymentsSQL reads each row through to_jsonb, so a column an older
// schema lacks reads as empty instead of failing. Connection passwords and
// plans are never selected.
const deploymentsSQL = `/* pg_sage agentdb_decommission v1 */
SELECT j->>'deployment_id', COALESCE(j->>'tenant_id', ''),
       lower(COALESCE(NULLIF(j->>'provider', ''), 'local_postgres')),
       lower(COALESCE(j->>'provisioning_level', j->>'isolation_type', '')),
       COALESCE(j->>'status', ''), COALESCE(j->>'provisioning_status', ''),
       COALESCE(j->>'database_name', ''), COALESCE(j->>'schema_name', ''),
       COALESCE(j->>'provider_resource_id', ''), COALESCE(j->>'create_operation_id', ''),
       COALESCE(NULLIF(j->>'secret_ref', ''), j->'connection_info'->>'secret_ref', ''),
       COALESCE((j->>'live_mode')::boolean, false),
       COALESCE(j->'metadata'->>'disposable' = 'true', false),
       COALESCE((j->>'created_at')::timestamptz, now()),
       COALESCE(j->'metadata'->'provider_params'->>'region', ''),
       COALESCE(j->'metadata'->'provider_params'->>'project', ''),
       COALESCE(j->'metadata'->'provider_params'->>'organization', ''),
       COALESCE(j->'metadata'->'provider_params'->>'mode', ''),
       COALESCE(j->'metadata'->>'credential_scope', ''),
       COALESCE(j->'connection_info'->>'host', j->'connection_info'->>'endpoint', ''),
       COALESCE(j->'connection_info'->>'port', ''),
       COALESCE(j->'connection_info'->>'database', j->'connection_info'->>'dbname', '')
FROM sage.agent_db_deployments d CROSS JOIN LATERAL to_jsonb(d) AS j
ORDER BY 1`

func loadDeployments(ctx context.Context, q queryer) ([]deployment, error) {
	rows, err := q.Query(ctx, deploymentsSQL)
	if err != nil {
		return nil, fmt.Errorf("read legacy deployments: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (deployment, error) {
		var d deployment
		err := row.Scan(&d.ID, &d.Tenant, &d.Provider, &d.Level, &d.Status,
			&d.ProvisioningStatus, &d.DatabaseName, &d.SchemaName, &d.ResourceID,
			&d.CreateOperationID, &d.SecretRef, &d.LiveMode, &d.Disposable, &d.CreatedAt,
			&d.Region, &d.Project, &d.Organization, &d.Mode, &d.CredentialScope,
			&d.Host, &d.Port, &d.ConnDatabase)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("read legacy deployments: %w", err)
	}
	return out, nil
}

func loadAcks(ctx context.Context, q queryer, acked map[string]bool) error {
	rows, err := q.Query(ctx, `/* pg_sage agentdb_decommission v1 */
		SELECT resource_id FROM sage.agentdb_decommission`)
	if err != nil {
		return fmt.Errorf("read acknowledgements: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read acknowledgements: %w", err)
	}
	for _, id := range ids {
		acked[id] = true
	}
	return nil
}
