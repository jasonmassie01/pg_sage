package autonomy

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// postgresSchemaContractSource reads every table contract a scan needs in
// one query (dogfood lifeos: one query per invariant per cycle).
type postgresSchemaContractSource struct{ pool *pgxpool.Pool }

func (s postgresSchemaContractSource) Contracts(
	ctx context.Context, invariants []schemaguard.Invariant,
) (map[string]schemaguard.TableContract, error) {
	result := map[string]schemaguard.TableContract{}
	if len(invariants) == 0 {
		return result, nil
	}
	schemas, tables := distinctTables(invariants)
	rows, err := s.pool.Query(ctx, tableContractsSQL, schemas, tables)
	if err != nil {
		return nil, fmt.Errorf("read table contracts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var schemaName, tableName string
		var contract schemaguard.TableContract
		var retentionSeconds *float64
		var exemptions []byte
		if err := rows.Scan(&schemaName, &tableName, &contract.AppendOnly,
			&retentionSeconds, &contract.RetentionColumn, &contract.ExpectedPrimaryKey,
			&exemptions); err != nil {
			return nil, fmt.Errorf("scan table contract: %w", err)
		}
		if retentionSeconds != nil {
			contract.RetentionWindow = time.Duration(*retentionSeconds * float64(time.Second))
		}
		if err := json.Unmarshal(exemptions, &contract.Exemptions); err != nil {
			return nil, fmt.Errorf("decode table contract exemptions for %s.%s: %w",
				schemaName, tableName, err)
		}
		result[schemaName+"."+tableName] = contract
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate table contracts: %w", err)
	}
	return result, nil
}

func distinctTables(invariants []schemaguard.Invariant) ([]string, []string) {
	seen := map[[2]string]bool{}
	schemas := make([]string, 0, len(invariants))
	tables := make([]string, 0, len(invariants))
	for _, invariant := range invariants {
		key := [2]string{invariant.Schema, invariant.Table}
		if seen[key] {
			continue
		}
		seen[key] = true
		schemas = append(schemas, invariant.Schema)
		tables = append(tables, invariant.Table)
	}
	return schemas, tables
}

// postgresSchemaHistorySource reads the recorded history of every
// invariant in a scan in one query whose cost does not grow with the
// ledger (dogfood lifeos: 12.7 s a scan over 252,974 legacy rows): one
// probe of idx_decision_schema_guard_key per scanned identity, and the
// counted rows by target through idx_decision_schema_guard_counted.
type postgresSchemaHistorySource struct{ pool *pgxpool.Pool }

func (s postgresSchemaHistorySource) History(
	ctx context.Context, invariants []schemaguard.Invariant,
) (schemaguard.HistoryIndex, error) {
	index := schemaguard.HistoryIndex{
		ByTarget: map[schemaguard.HistoryKey]schemaguard.History{},
		LastHash: map[string]string{},
	}
	if len(invariants) == 0 {
		return index, nil
	}
	rows, err := s.pool.Query(ctx, schemaHistorySQL, distinctTargets(invariants),
		distinctIdentities(invariants))
	if err != nil {
		return index, fmt.Errorf("read schema remediation history: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, first, second, hash string
		var dryRuns, reversions int
		if err := rows.Scan(&kind, &first, &second, &dryRuns, &reversions,
			&hash); err != nil {
			return index, fmt.Errorf("scan schema remediation history: %w", err)
		}
		if kind == "k" {
			index.LastHash[first] = hash
			continue
		}
		index.ByTarget[schemaguard.HistoryKey{
			Kind: schemaguard.InvariantKind(first), Target: second,
		}] = schemaguard.History{
			SuccessfulRetentionDryRuns: dryRuns, ExternalReversions: reversions,
		}
	}
	if err := rows.Err(); err != nil {
		return index, fmt.Errorf("iterate schema remediation history: %w", err)
	}
	return index, nil
}

func distinctTargets(invariants []schemaguard.Invariant) []string {
	seen := make(map[string]bool, len(invariants))
	targets := make([]string, 0, len(invariants))
	for _, invariant := range invariants {
		target := invariant.Target()
		if !seen[target] {
			seen[target] = true
			targets = append(targets, target)
		}
	}
	return targets
}

func distinctIdentities(invariants []schemaguard.Invariant) []string {
	seen := make(map[string]bool, len(invariants))
	identities := make([]string, 0, len(invariants))
	for _, invariant := range invariants {
		identity := schemaguard.InvariantIdentity(invariant)
		if !seen[identity] {
			seen[identity] = true
			identities = append(identities, identity)
		}
	}
	return identities
}

const tableContractsSQL = `/* pg_sage */
SELECT DISTINCT ON (tc.schema_name, tc.table_name)
       tc.schema_name, tc.table_name, tc.append_only,
       EXTRACT(epoch FROM tc.retention_interval)::float8,
       COALESCE(tc.retention_column,''), COALESCE(tc.expected_pk,''), tc.exemptions
FROM sage.table_contract tc
JOIN unnest($1::text[], $2::text[]) AS wanted(schema_name, table_name)
  ON wanted.schema_name = tc.schema_name AND wanted.table_name = tc.table_name
ORDER BY tc.schema_name, tc.table_name, tc.updated_at DESC, tc.id DESC`

// schemaHistorySQL returns two kinds of rows for the scan:
//
//   - 'h' rows count retention dry runs and external reversions per intent
//     and target ($1). A family row counts for every member it lists. Only
//     those rows are read (idx_decision_schema_guard_counted): the
//     recommendations and parks that make up the ledger never count.
//   - 'k' rows give the decision hash last recorded for each scanned
//     identity ($2): its newest row, whatever members it lists, one probe of
//     idx_decision_schema_guard_key each.
const schemaHistorySQL = `/* pg_sage */
SELECT 'h', d.intent, t.target,
       (count(*) FILTER (WHERE d.evidence->>'disposition' = 'dry_run'))::int,
       (count(*) FILTER (WHERE d.evidence->>'external_reversion' = 'true'))::int,
       ''
FROM sage.decision d
CROSS JOIN LATERAL jsonb_array_elements_text(d.target_objects) AS t(target)
WHERE d.feature = 'schema_guard'
  AND (d.evidence->>'disposition' = 'dry_run' OR d.evidence->>'external_reversion' = 'true')
  AND d.target_objects ?| $1::text[]
  AND t.target = ANY($1::text[])
GROUP BY d.intent, t.target
UNION ALL
SELECT 'k', wanted.key, '', 0, 0, latest.hash
FROM unnest($2::text[]) AS wanted(key)
CROSS JOIN LATERAL (
    SELECT COALESCE(d.evidence->>'decision_hash', '') AS hash
    FROM sage.decision d
    WHERE d.feature = 'schema_guard' AND d.evidence->>'invariant_key' = wanted.key
    ORDER BY d.id DESC
    LIMIT 1
) latest`
