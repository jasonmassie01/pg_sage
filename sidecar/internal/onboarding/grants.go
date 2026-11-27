package onboarding

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/rolegrants"
)

// GrantStatus is what pg_sage's role may do in the monitored database.
// Nil pointers were not checked.
type GrantStatus struct {
	Role          string `json:"role"`
	Superuser     bool   `json:"superuser"`
	Monitor       *bool  `json:"pg_monitor,omitempty"`
	ReadAllStats  *bool  `json:"pg_read_all_stats,omitempty"`
	SageSchema    *bool  `json:"sage_schema,omitempty"`
	SignalBackend *bool  `json:"pg_signal_backend,omitempty"`
	Maintain      *bool  `json:"pg_maintain,omitempty"`
	AlterSystem   *bool  `json:"alter_system,omitempty"`
	// SchemaCreate is CREATE on the schemas holding user tables.
	SchemaCreate *rolegrants.SchemaCreate `json:"schema_create,omitempty"`
	TablesOwned  int                      `json:"tables_owned"`
	TablesTotal  int                      `json:"tables_total"`
	// Owners are the roles owning tables whose privileges the role lacks,
	// most tables first, at most maxOwnerRoles; OwnerRoles counts them all.
	// These and the probes below feed the guide only: the API shape stays.
	Owners     []TableOwner `json:"-"`
	OwnerRoles int          `json:"-"`
	// Sequences is SELECT on the sequences of the user schemas.
	Sequences *rolegrants.SequenceSelect `json:"-"`
	// Signal is the pg_signal_backend probe SignalBackend is read from.
	Signal *rolegrants.SignalBackend `json:"-"`
}

// grantsSQL reads the role's memberships and ownership from the catalog;
// a predefined role missing on this version (pg_maintain before 17)
// yields NULL.
const grantsSQL = `/* pg_sage onboarding */ SELECT current_user::text, r.rolsuper,
  (SELECT pg_catalog.pg_has_role(current_user, oid, 'USAGE') FROM pg_catalog.pg_roles
   WHERE rolname = 'pg_monitor'),
  (SELECT pg_catalog.pg_has_role(current_user, oid, 'USAGE') FROM pg_catalog.pg_roles
   WHERE rolname = 'pg_read_all_stats'),
  COALESCE((SELECT pg_catalog.pg_has_role(current_user, n.nspowner, 'USAGE')
              OR pg_catalog.has_schema_privilege(current_user, n.oid, 'CREATE')
            FROM pg_catalog.pg_namespace n WHERE n.nspname = 'sage'), false),
  (SELECT pg_catalog.pg_has_role(current_user, oid, 'USAGE') FROM pg_catalog.pg_roles
   WHERE rolname = 'pg_maintain'),
  current_setting('server_version_num')::int
FROM pg_catalog.pg_roles r WHERE r.rolname = current_user`

// userTables picks the user tables of pg_class c in namespace n: ordinary
// or partitioned, outside the system schemas and the sage schema, and not
// part of an extension (whose owner the operator does not choose), the
// same tables rolegrants' schema CREATE check reads.
const userTables = `c.relkind IN ('r', 'p')
  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'sage')
  AND n.nspname !~ '^pg_toast' AND n.nspname !~ '^pg_temp'
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d
    WHERE d.classid = 'pg_catalog.pg_class'::pg_catalog.regclass
      AND d.objid = c.oid AND d.deptype = 'e')`

const tablesOwnedSQL = `/* pg_sage onboarding */ SELECT
  count(*) FILTER (WHERE pg_catalog.pg_has_role(current_user, c.relowner, 'USAGE'))::int,
  count(*)::int
FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE ` + userTables

// CheckGrants reads what the connected role may do; it needs no superuser.
func CheckGrants(ctx context.Context, pool *pgxpool.Pool) (GrantStatus, error) {
	if pool == nil {
		return GrantStatus{}, ErrNoPool
	}
	var g GrantStatus
	var sage bool
	var version int
	if err := pool.QueryRow(ctx, grantsSQL).Scan(&g.Role, &g.Superuser, &g.Monitor,
		&g.ReadAllStats, &sage, &g.Maintain, &version); err != nil {
		return GrantStatus{}, fmt.Errorf("read role grants: %w", err)
	}
	g.SageSchema = &sage
	if err := pool.QueryRow(ctx, tablesOwnedSQL).Scan(&g.TablesOwned,
		&g.TablesTotal); err != nil {
		return GrantStatus{}, fmt.Errorf("read table ownership: %w", err)
	}
	alter, err := alterSystemAllowed(ctx, pool, g.Superuser, version)
	if err != nil {
		return GrantStatus{}, err
	}
	g.AlterSystem = &alter
	if err := g.readShared(ctx, pool); err != nil {
		return GrantStatus{}, err
	}
	return g, nil
}

// readShared reads the probes the startup check shares (rolegrants) and
// the owner roles of the tables the role does not own.
func (g *GrantStatus) readShared(ctx context.Context, pool *pgxpool.Pool) error {
	sc, err := rolegrants.CheckSchemaCreate(ctx, pool)
	if err != nil {
		return err
	}
	g.SchemaCreate = &sc
	sig, err := rolegrants.CheckSignalBackend(ctx, pool)
	if err != nil {
		return err
	}
	usable := sig.Usable
	g.Signal, g.SignalBackend = &sig, &usable
	seq, err := rolegrants.CheckSequenceSelect(ctx, pool)
	if err != nil {
		return err
	}
	g.Sequences = &seq
	g.Owners, g.OwnerRoles, err = readTableOwners(ctx, pool)
	return err
}

// alterSystemAllowed: superusers may; from PostgreSQL 15 a role may be
// granted ALTER SYSTEM per parameter (work_mem stands for the tunables).
func alterSystemAllowed(ctx context.Context, pool *pgxpool.Pool, super bool,
	version int) (bool, error) {
	if super || version < 150000 {
		return super, nil
	}
	var ok bool
	if err := pool.QueryRow(ctx, `/* pg_sage onboarding */ SELECT
		pg_catalog.has_parameter_privilege(current_user, 'work_mem', 'ALTER SYSTEM')`).
		Scan(&ok); err != nil {
		return false, fmt.Errorf("read ALTER SYSTEM privilege: %w", err)
	}
	return ok, nil
}
