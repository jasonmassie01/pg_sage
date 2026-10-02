package earned

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Legacy levels (P0-5 migration decision, recorded in
// reviews/2026-10-02-p0-trust-ledger-report.md): a level stored before
// the ledger was per database, whose database could not be determined,
// stays a deployment-wide row (database_name ''). Each database adopts it
// once, at its current level, when it binds, unless it already has its
// own row. The legacy row stays for databases that bind later. An adopted
// earned level is still capped by the adopting database's own evidence at
// the gate, so it grants nothing that database has not earned; an adopted
// restriction keeps holding. Carried-over levels are not adopted: each
// database seeds its own from its own configuration (SeedCarriedOver).

// legacyDatabase marks a deployment-wide row from before database scope.
const legacyDatabase = ""

// AdoptLegacy copies the deployment's legacy levels into this database's
// ledger where it has none, records each adoption and returns the
// adopted states.
func (s *Service) AdoptLegacy(ctx context.Context) ([]State, error) {
	adopted := []State{}
	err := s.store.withTx(ctx, func(tx pgx.Tx) error {
		adopted = adopted[:0]
		states, err := s.store.adoptLegacyRows(ctx, tx, s.now())
		if err != nil {
			return err
		}
		for _, st := range states {
			evidence, err := json.Marshal(map[string]any{"from": "deployment-wide level",
				"version": st.Version})
			if err != nil {
				return fmt.Errorf("encode adoption evidence: %w", err)
			}
			if err := s.store.appendEvent(ctx, tx, Event{Family: st.Family, Class: st.Class,
				Type: EventDatabaseScoped, From: levelPtr(defaultLevel(st.Family)),
				To: levelPtr(st.Level), Actor: ActorPgSage, Reason: "adopted the " +
					"deployment-wide level stored before the ledger was per database",
				Evidence: evidence, At: st.ChangedAt}); err != nil {
				return err
			}
			adopted = append(adopted, st)
		}
		return nil
	})
	s.invalidate()
	if err != nil {
		return nil, err
	}
	return adopted, nil
}

// adoptLegacyRows inserts this database's copy of every legacy ledger
// row it has no row for, returning the inserted states.
func (s *PostgresStore) adoptLegacyRows(ctx context.Context, q querier, at time.Time) (
	[]State, error) {
	rows, err := q.Query(ctx, `INSERT INTO sage.sre_family_autonomy
		(deployment_id, database_name, family, action_class, level, evidence, changed_by,
		 change_reason, changed_at)
		SELECT deployment_id, $2::text, family, action_class, level, evidence, changed_by,
		       'adopted for this database: ' || left(change_reason, 950), $4::timestamptz
		FROM sage.sre_family_autonomy
		WHERE deployment_id = $1 AND database_name = $3 AND provenance = 'ledger'
		ON CONFLICT (deployment_id, database_name, family, action_class) DO NOTHING
		RETURNING `+levelColumns, s.deployment, s.database, legacyDatabase, at)
	if err != nil {
		return nil, storeErr("adopt legacy levels", err)
	}
	defer rows.Close()
	var out []State
	for rows.Next() {
		st, err := scanState(rows)
		if err != nil {
			return nil, storeErr("scan adopted level", err)
		}
		out = append(out, st)
	}
	return out, storeErr("adopt legacy levels", rows.Err())
}
