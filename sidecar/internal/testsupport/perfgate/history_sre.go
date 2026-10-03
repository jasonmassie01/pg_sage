package perfgate

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// sreStep inserts one Sage SRE table's history. args picks its bound
// parameters from (rows, deployment, own database id).
type sreStep struct {
	table string
	full  bool // seeded with at least HistoryRows rows (GrowingTables)
	sql   string
	args  func(n int, own Binding) []any
}

func argsNDB(n int, own Binding) []any { return []any{n, own.DeploymentID, own.DatabaseID} }
func argsND(n int, own Binding) []any  { return []any{n, own.DeploymentID} }
func argsNone(int, Binding) []any      { return nil }

// seedSRE binds the runtime's identity (and three other fleet databases)
// and seeds finished investigations with their steps, evidence, events,
// hypotheses and outcomes, plus the change feed, SLI samples and the
// autonomy ledgers. 70% of the per-database history is the runtime's.
func seedSRE(ctx context.Context, pool *pgxpool.Pool, n int, own Binding) error {
	if _, err := pool.Exec(ctx, `INSERT INTO sage.sre_deployments (deployment_id)
		VALUES ($1::uuid) ON CONFLICT (singleton) DO NOTHING`, own.DeploymentID); err != nil {
		return fmt.Errorf("perfgate: seed sre deployment: %w", err)
	}
	var dep string
	if err := pool.QueryRow(ctx, `/* `+HarnessTag+` */ SELECT deployment_id::text
		FROM sage.sre_deployments`).Scan(&dep); err != nil {
		return fmt.Errorf("perfgate: read sre deployment: %w", err)
	}
	if dep != own.DeploymentID {
		return fmt.Errorf("perfgate: sre deployment %s already exists (binding wants %s)",
			dep, own.DeploymentID)
	}
	for _, step := range sreSteps {
		if _, err := pool.Exec(ctx, step.sql, step.args(n, own)...); err != nil {
			return fmt.Errorf("perfgate: seed sage.%s: %w", step.table, err)
		}
	}
	return nil
}

// investigationTS spreads investigation g of ceil($1/4) over 7 days.
const investigationTS = `now() - (g::double precision / (($1::int + 3) / 4)) * interval '7 days'`

var sreSteps = []sreStep{
	{"sre_database_bindings", false, `INSERT INTO sage.sre_database_bindings (deployment_id,
		database_id, runtime_key, identity_strength, cluster_epoch)
		SELECT $1::uuid, $2::uuid, $3, 'configured', 'unknown'
		UNION ALL SELECT $1::uuid, gen_random_uuid(), 'fleet:perfgate_other_' || k,
		'configured', 'unknown' FROM generate_series(1, 3) k
		ON CONFLICT DO NOTHING`, func(_ int, own Binding) []any {
		return []any{own.DeploymentID, own.DatabaseID, own.RuntimeKey}
	}},
	{"sre_investigations", false, `WITH b AS (SELECT array_agg(database_id ORDER BY runtime_key)
		AS others FROM sage.sre_database_bindings
		WHERE deployment_id = $2::uuid AND database_id <> $3::uuid)
		INSERT INTO sage.sre_investigations (deployment_id, database_id, id, source_case_id,
		trigger_kind, trigger_fingerprint, state, version, fence_token, created_at,
		updated_at, expires_at, subject, summary, concluded_at)
		SELECT $2::uuid, CASE WHEN g % 10 < 7 THEN $3::uuid ELSE b.others[1 + g % 3] END,
		gen_random_uuid(), 'perfgate-' || g,
		(ARRAY['lock_blocking','connection_pressure','wal_retention','plan_regression'])
		[1 + g % 4], sha256(('perfgate-' || g)::bytea),
		(ARRAY['concluded','inconclusive','expired','cancelled'])[1 + g % 4], 3, 1,
		t.ts, t.ts + interval '2 minutes', t.ts + interval '1 day', 'perfgate', '{}',
		CASE WHEN g % 4 < 2 THEN t.ts + interval '2 minutes' END
		FROM generate_series(1, ($1::int + 3) / 4) g CROSS JOIN b,
		LATERAL (SELECT ` + investigationTS + ` AS ts) t`, argsNDB},
	{"sre_steps", true, `INSERT INTO sage.sre_steps (deployment_id, database_id, investigation_id,
		id, sequence, idempotency_key, fence_token, probe_count, next_state, created_at)
		SELECT i.deployment_id, i.database_id, i.id, gen_random_uuid(), s, 'step-' || s, 1, 1,
		'collecting', i.created_at + s * interval '10 seconds'
		FROM sage.sre_investigations i CROSS JOIN generate_series(1, 4) s
		WHERE i.source_case_id LIKE 'perfgate-%'`, argsNone},
	{"sre_evidence", true, `INSERT INTO sage.sre_evidence (deployment_id, database_id,
		investigation_id, id, step_id, source_kind, probe_id, probe_version, observed_at,
		collected_at, capability_state, payload_version, classification, payload, sha256)
		SELECT st.deployment_id, st.database_id, st.investigation_id, gen_random_uuid(),
		st.id, 'probe', 'lock_chain', 'v1', st.created_at, st.created_at, 'available', 1,
		'redacted', '{"rows": []}', sha256(st.id::text::bytea)
		FROM sage.sre_steps st WHERE st.idempotency_key LIKE 'step-%'`, argsNone},
	{"sre_events", true, `INSERT INTO sage.sre_events (deployment_id, database_id,
		investigation_id, sequence, event_type, actor, observed_at, payload, previous_hash, hash)
		SELECT i.deployment_id, i.database_id, i.id, s,
		(ARRAY['created','claimed','model_reviewed','concluded'])[s], 'perfgate',
		i.created_at + s * interval '10 seconds', '{}',
		CASE WHEN s > 1 THEN sha256((i.id::text || (s - 1))::bytea) END,
		sha256((i.id::text || s)::bytea)
		FROM sage.sre_investigations i CROSS JOIN generate_series(1, 4) s
		WHERE i.source_case_id LIKE 'perfgate-%'`, argsNone},
	{"sre_hypotheses", false, `INSERT INTO sage.sre_hypotheses (deployment_id, database_id,
		investigation_id, id, revision, ordinal, graph_version, family, node_id, label,
		mechanism, status, confidence, support, contradict, refutation_probe)
		SELECT i.deployment_id, i.database_id, i.id, gen_random_uuid(), 1, o, 'v1',
		'lock_blocking', 'node_' || o, 'perfgate hypothesis', '', 'unproven', 0.3, '[]', '[]',
		'lock_chain'
		FROM sage.sre_investigations i CROSS JOIN generate_series(1, 2) o
		WHERE i.source_case_id LIKE 'perfgate-%'`, argsNone},
	{"sre_investigation_outcomes", false, `INSERT INTO sage.sre_investigation_outcomes
		(deployment_id, database_id, investigation_id, id, verdict, actor, recorded_at)
		SELECT deployment_id, database_id, id, gen_random_uuid(), 'confirmed', 'perfgate',
		updated_at FROM sage.sre_investigations WHERE source_case_id LIKE 'perfgate-%'`,
		argsNone},
	{"sre_change_events", true, `INSERT INTO sage.sre_change_events (deployment_id, id,
		database_id, source, event_id, kind, summary, occurred_at, signature_status,
		change_hash)
		SELECT $2::uuid, gen_random_uuid(), CASE WHEN g % 10 < 7 THEN $3::uuid END,
		'perfgate', 'e' || g, (ARRAY['deploy','migration','config','ddl'])[1 + g % 4],
		'perfgate history', ` + spreadSRE + `, 'internal', sha256(('e' || g)::bytea)
		FROM generate_series(1, $1::int) g`, argsNDB},
	{"sre_sli_samples", true, `INSERT INTO sage.sre_sli_samples (deployment_id, slo_name, series,
		observed_at, bad, eligible)
		SELECT $2::uuid, 'availability', 's' || g % 50, now() - g * interval '1 second', 0, 100
		FROM generate_series(1, $1::int) g`, argsND},
	{"sre_autonomy_events", true, `INSERT INTO sage.sre_autonomy_events (deployment_id, family,
		action_class, event_type, from_level, to_level, actor, reason, database_name,
		created_at)
		SELECT $2::uuid, ` + seedFamily + `, ` + seedActionClass + `,
		(ARRAY['promotion_proposed','downgraded','capped','cap_cleared'])[1 + g % 4], 0, 1,
		'perfgate', 'perfgate history', current_database(), ` + spreadSRE + `
		FROM generate_series(1, $1::int) g`, argsND},
	{"sre_autonomy_outcomes", true, `INSERT INTO sage.sre_autonomy_outcomes (deployment_id,
		database_name, family, action_class, level, result, source, actor, recorded_at)
		SELECT $2::uuid, current_database(), ` + seedFamily + `, ` + seedActionClass + `, 1,
		(ARRAY['verified_recovery','not_recovered'])[1 + g % 2], 'bench', 'perfgate',
		` + spreadSRE + `
		FROM generate_series(1, $1::int) g`, argsND},
}

// spreadSRE spreads row g of $1 over 7 days, newest first.
const spreadSRE = `now() - (g::double precision / $1::int) * interval '7 days'`

// seedFamily and seedActionClass spread ledger rows over twelve incident
// families and four action classes, as a fleet's history would be.
const (
	seedFamily = `(ARRAY['lock_blocking','connection_pressure','wal_retention',
		'plan_regression','checkpoint_storm','temp_file_explosion','replication_lag',
		'lwlock_contention','wraparound_runway','disk_wal_runway','sequence_runway',
		'table_bloat'])[1 + g % 12]`
	seedActionClass = `(ARRAY['cancel_query','terminate_backend','vacuum_freeze',
		'drop_slot'])[1 + g % 4]`
)
