package agentguard

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PreflightResult is the PUBLIC preflight of one database (§6.6). Every
// agent role is a member of PUBLIC, so what PUBLIC may do, an agent may do.
type PreflightResult struct {
	// PublicCreate lists schemas of the profile path where PUBLIC has
	// CREATE (P1): grants are refused until it is revoked (G1-16).
	PublicCreate []string `json:"public_create"`
	// PublicConnect lists other databases of the cluster PUBLIC may connect
	// to (P2); it refuses the direct lane only (G3).
	PublicConnect []string `json:"public_connect"`
	// PublicDefinerFunctions lists volatile SECURITY DEFINER functions
	// PUBLIC may execute in a reachable schema (P3); direct lane only.
	PublicDefinerFunctions []string `json:"public_definer_functions"`
	// Baseline is how many PUBLIC privileges were recorded (P4).
	Baseline int `json:"baseline"`
}

// GrantsAllowed is nil when P1 passes, else a DeniedError naming the exact
// REVOKE statements an owner must run.
func (r PreflightResult) GrantsAllowed() error {
	if len(r.PublicCreate) == 0 {
		return nil
	}
	fixes := make([]string, len(r.PublicCreate))
	for i, s := range r.PublicCreate {
		fixes[i] = "REVOKE CREATE ON SCHEMA " + ident(s) + " FROM PUBLIC;"
	}
	return &DeniedError{Reason: ReasonPublicCreate,
		Detail: "PUBLIC can create objects in " + strings.Join(r.PublicCreate, ", ") +
			"; an agent could create and own objects there",
		Fix: strings.Join(fixes, "\n")}
}

// publicCreateSQL finds schemas where PUBLIC holds CREATE, from the ACL or
// the owner default. $1 nil checks every non-system schema.
const publicCreateSQL = `/* pg_sage guard_preflight v1 */
SELECT n.nspname::text FROM pg_catalog.pg_namespace n
WHERE ($1::text[] IS NULL OR n.nspname = ANY($1))
  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
  AND n.nspname !~ '^pg_(toast|temp_|toast_temp_)'
  AND EXISTS (SELECT 1 FROM pg_catalog.aclexplode(COALESCE(n.nspacl,
                pg_catalog.acldefault('n', n.nspowner))) a
              WHERE a.grantee = 0 AND a.privilege_type = 'CREATE')
ORDER BY 1`

const publicConnectSQL = `/* pg_sage guard_preflight v1 */
SELECT d.datname::text FROM pg_catalog.pg_database d
WHERE d.datallowconn AND NOT d.datistemplate
  AND d.datname <> pg_catalog.current_database()
  AND EXISTS (SELECT 1 FROM pg_catalog.aclexplode(COALESCE(d.datacl,
                pg_catalog.acldefault('d', d.datdba))) a
              WHERE a.grantee = 0 AND a.privilege_type = 'CONNECT')
ORDER BY 1`

const publicDefinerSQL = `/* pg_sage guard_preflight v1 */
SELECT p.oid::pg_catalog.regprocedure::text FROM pg_catalog.pg_proc p
JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
WHERE p.prosecdef AND p.provolatile = 'v'
  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
  AND EXISTS (SELECT 1 FROM pg_catalog.aclexplode(COALESCE(n.nspacl,
                pg_catalog.acldefault('n', n.nspowner))) s
              WHERE s.grantee = 0 AND s.privilege_type = 'USAGE')
  AND EXISTS (SELECT 1 FROM pg_catalog.aclexplode(COALESCE(p.proacl,
                pg_catalog.acldefault('f', p.proowner))) a
              WHERE a.grantee = 0 AND a.privilege_type = 'EXECUTE')
ORDER BY 1 LIMIT 200`

// Preflight runs P1–P4 on pool's database before the first grant there,
// recording the PUBLIC baseline (P4) in its sage schema. schemas is the
// profile path; empty checks every non-system schema for P1.
func Preflight(ctx context.Context, pool *pgxpool.Pool, schemas []string) (PreflightResult,
	error) {
	if pool == nil {
		return PreflightResult{}, invalid("no database pool for the preflight")
	}
	var res PreflightResult
	var path any
	if len(schemas) > 0 {
		path = schemas
	}
	var err error
	if res.PublicCreate, err = queryStrings(ctx, pool, publicCreateSQL, path); err != nil {
		return PreflightResult{}, err
	}
	if res.PublicConnect, err = queryStrings(ctx, pool, publicConnectSQL); err != nil {
		return PreflightResult{}, err
	}
	if res.PublicDefinerFunctions, err = queryStrings(ctx, pool,
		publicDefinerSQL); err != nil {
		return PreflightResult{}, err
	}
	res.Baseline, err = RecordPublicBaseline(ctx, pool)
	return res, err
}

func queryStrings(ctx context.Context, q Querier, sql string, args ...any) ([]string,
	error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("agentguard: preflight: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("agentguard: preflight: %w", err)
	}
	return out, nil
}
