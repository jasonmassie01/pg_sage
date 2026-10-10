package decommission

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// labelled is one (deployment, value) pair read from an evidence table.
type labelled struct{ deployment, value string }

func queryLabelled(ctx context.Context, q queryer, what, sql string) ([]labelled, error) {
	rows, err := q.Query(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", what, err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (labelled, error) {
		var l labelled
		err := row.Scan(&l.deployment, &l.value)
		return l, err
	})
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", what, err)
	}
	return out, nil
}

const (
	liveReceiptsSQL = `/* pg_sage agentdb_decommission v1 */
SELECT a.deployment_id, COALESCE(r.payload->>'ProviderResourceID', '')
FROM sage.agent_db_live_receipts r
JOIN sage.agent_db_live_authorizations a USING (authorization_id)`
	consumedSQL = `/* pg_sage agentdb_decommission v1 */
SELECT deployment_id, operation FROM sage.agent_db_live_authorizations
WHERE consumed_at IS NOT NULL`
	// Every attempt kind that called a provider for real.
	liveAttemptsSQL = `/* pg_sage agentdb_decommission v1 */
SELECT DISTINCT deployment_id, kind FROM sage.agent_db_provision_attempts
WHERE kind LIKE '%\_live' OR kind = 'live_reconcile_status'`
)

// loadEvidence reads live-call evidence held outside the deployment row: a
// live receipt, a consumed authorization, a live provider attempt and a live
// creation receipt. Each table is read only when it exists.
func loadEvidence(ctx context.Context, q queryer, present map[string]bool,
	st *legacyState) error {
	add := func(dep, label string) { st.evidence[dep] = append(st.evidence[dep], label) }
	if present["agent_db_live_receipts"] && present["agent_db_live_authorizations"] {
		got, err := queryLabelled(ctx, q, "live receipts", liveReceiptsSQL)
		if err != nil {
			return err
		}
		for _, l := range got {
			add(l.deployment, "live_receipt")
			if l.value != "" {
				st.liveReceipts[l.deployment] = l.value
			}
		}
	}
	if present["agent_db_live_authorizations"] {
		got, err := queryLabelled(ctx, q, "consumed authorizations", consumedSQL)
		if err != nil {
			return err
		}
		for _, l := range got {
			add(l.deployment, "consumed_authorization:"+l.value)
		}
	}
	if present["agent_db_provision_attempts"] {
		got, err := queryLabelled(ctx, q, "live provider attempts", liveAttemptsSQL)
		if err != nil {
			return err
		}
		for _, l := range got {
			add(l.deployment, "attempt:"+l.value)
		}
	}
	if present["agent_db_creation_receipts"] {
		return loadCreationReceipts(ctx, q, st)
	}
	return nil
}

func loadCreationReceipts(ctx context.Context, q queryer, st *legacyState) error {
	rows, err := q.Query(ctx, `/* pg_sage agentdb_decommission v1 */
		SELECT deployment_id, provider_resource_id, region, account_ref,
		       operation_mode = 'live'
		FROM sage.agent_db_creation_receipts`)
	if err != nil {
		return fmt.Errorf("read creation receipts: %w", err)
	}
	type row struct {
		dep string
		r   creationReceipt
	}
	got, err := pgx.CollectRows(rows, func(cr pgx.CollectableRow) (row, error) {
		var x row
		err := cr.Scan(&x.dep, &x.r.ResourceID, &x.r.Region, &x.r.Account, &x.r.Live)
		return x, err
	})
	if err != nil {
		return fmt.Errorf("read creation receipts: %w", err)
	}
	for _, x := range got {
		if x.r.Live {
			st.evidence[x.dep] = append(st.evidence[x.dep], "live_creation_receipt")
			st.receipts[x.dep] = x.r
			continue
		}
		// A dry-run receipt is no evidence and its resource id is not real;
		// only its region and account may fill in the gaps.
		st.receipts[x.dep] = creationReceipt{Region: x.r.Region, Account: x.r.Account}
	}
	return nil
}

// loadLocalArtifacts checks which local schemas and databases pg_sage
// created still exist on the control database's server.
func loadLocalArtifacts(ctx context.Context, q queryer, st *legacyState) error {
	var schemas, databases []string
	for _, d := range st.deployments {
		if d.Provider != providerLocal || d.CredentialScope == "" {
			continue
		}
		schemas = append(schemas, d.CredentialScope)
		databases = append(databases, d.CredentialScope)
	}
	if len(schemas) == 0 {
		return nil
	}
	got, err := queryNames(ctx, q, "local schemas", `/* pg_sage agentdb_decommission v1 */
		SELECT nspname FROM pg_catalog.pg_namespace WHERE nspname = ANY($1)`, schemas)
	if err != nil {
		return err
	}
	for _, n := range got {
		st.schemas[n] = true
	}
	got, err = queryNames(ctx, q, "local databases", `/* pg_sage agentdb_decommission v1 */
		SELECT datname FROM pg_catalog.pg_database WHERE datname = ANY($1)`, databases)
	if err != nil {
		return err
	}
	for _, n := range got {
		st.databases[n] = true
	}
	return nil
}

func queryNames(ctx context.Context, q queryer, what, sql string, names []string) (
	[]string, error) {
	rows, err := q.Query(ctx, sql, names)
	if err != nil {
		return nil, fmt.Errorf("look up %s: %w", what, err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("look up %s: %w", what, err)
	}
	return out, nil
}
