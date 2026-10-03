package perfgate

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// The schema guard's legacy ledger (dogfood lifeos, v1.8.3): v1.8.1 wrote
// one sage.decision row per invariant per cycle for ~160 leaked test_*
// schemas, each row naming up to 71 of them. The history read matched
// nearly every legacy row and re-aggregated them all on every structural
// scan (12.7 s a call). The fixture keeps that shape: an idle family of
// leaked copies, each with one all-text table (an everything_text
// invariant), and a flood of legacy rows naming them.

const (
	leakedCloneSchemas  = 80
	leakedTargetsPerRow = 70
	leakedTable         = "leak_events"
	// legacyGuardShare: legacy rows are HistoryRows / legacyGuardShare
	// (5,000 small, 37,500 large).
	legacyGuardShare = 4
)

// leakedSchema is the i-th leaked copy; its six-digit suffix makes the
// copies one clone family.
func leakedSchema(i int) string { return fmt.Sprintf("test_leak_%06x", 0xa00000+i) }

func leakedSchemas() []string {
	out := make([]string, leakedCloneSchemas)
	for i := range out {
		out[i] = leakedSchema(i)
	}
	return out
}

// buildLeakedClones creates the leaked copies (idempotent).
func buildLeakedClones(ctx context.Context, conn *pgxpool.Conn) error {
	for _, sch := range leakedSchemas() {
		schema := pgx.Identifier{sch}.Sanitize()
		table := pgx.Identifier{sch, leakedTable}.Sanitize()
		for _, ddl := range []string{"CREATE SCHEMA IF NOT EXISTS " + schema,
			"CREATE TABLE IF NOT EXISTS " + table + " (a text, b text, c text)"} {
			if _, err := conn.Exec(ctx, ddl); err != nil {
				return fmt.Errorf("perfgate: build leaked clone %s: %w", sch, err)
			}
		}
	}
	return nil
}

// leakedIdentity is the invariant identity the schema guard gives the
// copies' everything_text invariant, as the legacy rows recorded it.
func leakedIdentity() string {
	schemas := leakedSchemas()
	shapes := make([]schemaguard.SchemaShape, 0, len(schemas))
	for _, sch := range schemas {
		shapes = append(shapes, schemaguard.SchemaShape{Schema: sch,
			Tables: []string{leakedTable}})
	}
	family := schemaguard.GroupFamilies(shapes)[schemas[0]]
	return schemaguard.InvariantIdentity(schemaguard.Invariant{
		Kind: schemaguard.InvariantEverythingText, Schema: schemas[0], Table: leakedTable,
		Family: family})
}

// seedLegacyGuardFlood writes the legacy rows over the last 20 hours: row g
// names leakedTargetsPerRow copies in one of 11 windows (the family's
// membership drifted as test schemas came and went).
func seedLegacyGuardFlood(ctx context.Context, pool *pgxpool.Pool, s Scale) error {
	rows := max(s.HistoryRows/legacyGuardShare, 1)
	_, err := pool.Exec(ctx, legacyGuardFloodSQL, rows, leakedSchemas(),
		leakedTargetsPerRow, leakedIdentity(), leakedTable)
	if err != nil {
		return fmt.Errorf("perfgate: seed legacy schema guard rows: %w", err)
	}
	return nil
}

var legacyGuardFloodSQL = `/* ` + HarnessTag + ` */
WITH s AS (
    SELECT ord - 1 AS n, name FROM unnest($2::text[]) WITH ORDINALITY AS u(name, ord)
), windows AS (
    SELECT w.w, jsonb_agg(s.name || '.' || $5::text ORDER BY s.n) AS targets
    FROM generate_series(0, 10) AS w(w)
    JOIN s ON (s.n + w.w * 7) % cardinality($2::text[]) < $3::int
    GROUP BY w.w
)
INSERT INTO sage.decision (feature, intent, target_objects, verdict, risk_tier, reason,
    evidence, evidence_id, created_at)
SELECT 'schema_guard', 'everything_text', windows.targets, 'observe_only', 'moderate',
       'perfgate legacy schema guard flood',
       jsonb_build_object('route', 'clone_rehearsal', 'disposition', 'recommend',
           'invariant_key', $4::text, 'decision_hash', md5((g % 11)::text)),
       'perfgate-guard-' || g, ` + spread("20 hours") + `
FROM generate_series(1, $1) AS g
JOIN windows ON windows.w = g % 11`
