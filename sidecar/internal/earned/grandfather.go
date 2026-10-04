package earned

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Grandfathering (roadmap 1.2): before the unified ledger, the time ramp
// alone let self-initiated classes run unattended. On the first start of
// a database under the ledger, the level that ramp granted each class is
// kept as an explicitly labelled grandfathered level, so no autonomy is
// silently lost. It happens once per database (sage.trust_ledger_state):
// the ramp elapsing later grants nothing. Grandfathered levels demote
// like any other; an existing ledger row is never raised or lowered.

// GrandfatherReport is a database's grandfathering: when it happened,
// what it seeded, and whether this call did it (Migrated).
type GrandfatherReport struct {
	Database   string    `json:"database"`
	Migrated   bool      `json:"migrated"`
	MigratedAt time.Time `json:"migrated_at"`
	Seeded     []State   `json:"seeded"`
}

// GrandfatheredLevel is the level the time-ramp gate granted class on its
// own initiative under bound at now, with what granted it: L3 when it ran
// unattended, L2 when it queued for one-click approval, else L1. It is
// capped by the class (an irreversible class never exceeds L1).
func GrandfatheredLevel(bound policy.RuntimeState, now time.Time, c ActionClass) (Level,
	string) {
	spec, ok := selfSpec(c)
	if !ok || !bound.ExecutorEnabled || bound.TrustLevel == policy.TrustObservation ||
		(bound.TrustLevel != policy.TrustAdvisory && bound.TrustLevel != policy.TrustAutonomous) {
		return L1, ""
	}
	level, why := rampLevel(bound, now, spec)
	if limit := CapForPair(spec.family, c); level > limit {
		level = limit
	}
	if level <= L1 {
		return L1, ""
	}
	return level, why
}

// rampLevel mirrors the time-ramp gate's tier decision for one class.
func rampLevel(b policy.RuntimeState, now time.Time, spec selfClass) (Level, string) {
	head := fmt.Sprintf("trust.level=%s, execution_mode=%s", b.TrustLevel, b.ExecutionMode)
	switch b.ExecutionMode {
	case policy.ExecutionApproval:
		return L2, head + ": every action queued for one-click approval"
	case policy.ExecutionAuto:
	default:
		return L1, ""
	}
	rollback := rollbackOf(spec.class)
	if spec.tier == policy.RiskSafe {
		age := policy.RampAge(b.SafeRampAge, policy.SpecSafeRampAge, rollback)
		if b.Tier3Safe && rampElapsed(b, now, age) {
			return L3, fmt.Sprintf("%s, tier3_safe, ramp %s elapsed", head, humanDuration(age))
		}
		return L1, ""
	}
	if b.TrustLevel == policy.TrustAdvisory {
		return L2, head + ": moderate actions queued for one-click approval"
	}
	age := policy.RampAge(b.ModerateRampAge, policy.SpecModerateRampAge, rollback)
	if b.Tier3Moderate && rampElapsed(b, now, age) {
		return L3, fmt.Sprintf("%s, tier3_moderate, ramp %s elapsed", head, humanDuration(age))
	}
	return L1, ""
}

func rampElapsed(b policy.RuntimeState, now time.Time, age time.Duration) bool {
	return !b.RampStart.IsZero() && now.Sub(b.RampStart) >= age
}

// SeedGrandfathered grandfathers database's ramp autonomy under bound,
// once. A later call reports the earlier grandfathering and changes
// nothing, whatever bound says.
func (s *Service) SeedGrandfathered(ctx context.Context, database string,
	bound policy.RuntimeState) (GrandfatherReport, error) {
	if err := s.store.checkDatabase(database); err != nil {
		return GrandfatherReport{}, err
	}
	now := s.now()
	rep := GrandfatherReport{Database: database, Seeded: []State{}}
	err := s.store.withTx(ctx, func(tx pgx.Tx) error {
		rep.Migrated, rep.Seeded = false, []State{}
		at, err := s.store.lockGrandfatherMarker(ctx, tx)
		if err != nil || at != nil {
			if at != nil {
				rep.MigratedAt = at.UTC()
			}
			return err
		}
		for _, sc := range selfClasses {
			st, ok, err := s.seedOne(ctx, tx, bound, now, sc)
			if err != nil {
				return err
			}
			if ok {
				rep.Seeded = append(rep.Seeded, st)
			}
		}
		rep.Migrated, rep.MigratedAt = true, now
		return s.store.markGrandfathered(ctx, tx, bound, rep.Seeded, now)
	})
	s.invalidate()
	if err != nil {
		return GrandfatherReport{}, err
	}
	return rep, nil
}

func (s *Service) seedOne(ctx context.Context, tx pgx.Tx, bound policy.RuntimeState,
	now time.Time, sc selfClass) (State, bool, error) {
	level, why := GrandfatheredLevel(bound, now, sc.class)
	if level <= defaultLevel(sc.family) {
		return State{}, false, nil
	}
	return s.store.seedGrandfathered(ctx, tx, sc.family, sc.class, level, why, now)
}

// Grandfathered reads the database's grandfathering, nil before it.
func (s *Service) Grandfathered(ctx context.Context) (*GrandfatherReport, error) {
	var at *time.Time
	var seeded []byte
	err := s.store.pool.QueryRow(ctx, `SELECT grandfathered_at, seeded
		FROM sage.trust_ledger_state WHERE deployment_id = $1 AND database_name = $2`,
		s.store.deployment, s.store.database).Scan(&at, &seeded)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && at == nil) {
		return nil, nil
	}
	if err != nil {
		return nil, storeErr("read grandfathering", err)
	}
	rep := &GrandfatherReport{Database: s.store.database, MigratedAt: at.UTC(),
		Seeded: []State{}}
	if len(seeded) > 0 {
		if err := json.Unmarshal(seeded, &rep.Seeded); err != nil {
			return nil, fmt.Errorf("decode grandfathered levels: %w", err)
		}
	}
	return rep, nil
}

// lockGrandfatherMarker locks the database's state row (creating it) and
// returns when it was grandfathered, nil when it has not been.
func (s *PostgresStore) lockGrandfatherMarker(ctx context.Context, tx pgx.Tx) (*time.Time,
	error) {
	if _, err := tx.Exec(ctx, `INSERT INTO sage.trust_ledger_state
		(deployment_id, database_name) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		s.deployment, s.database); err != nil {
		return nil, storeErr("create trust ledger state", err)
	}
	var at *time.Time
	err := tx.QueryRow(ctx, `SELECT grandfathered_at FROM sage.trust_ledger_state
		WHERE deployment_id = $1 AND database_name = $2 FOR UPDATE`,
		s.deployment, s.database).Scan(&at)
	return at, storeErr("lock trust ledger state", err)
}

func (s *PostgresStore) markGrandfathered(ctx context.Context, tx pgx.Tx,
	bound policy.RuntimeState, seeded []State, at time.Time) error {
	b, err := json.Marshal(bound)
	if err != nil {
		return fmt.Errorf("encode grandfathering bound: %w", err)
	}
	list, err := json.Marshal(seeded)
	if err != nil {
		return fmt.Errorf("encode grandfathered levels: %w", err)
	}
	_, err = tx.Exec(ctx, `UPDATE sage.trust_ledger_state
		SET grandfathered_at = $3, bound = $4, seeded = $5, updated_at = clock_timestamp()
		WHERE deployment_id = $1 AND database_name = $2`,
		s.deployment, s.database, at, b, list)
	return storeErr("mark grandfathering", err)
}

// seedGrandfathered inserts a grandfathered level and its history entry
// when the pair has no row; ok is false when it already had one.
func (s *PostgresStore) seedGrandfathered(ctx context.Context, tx pgx.Tx, f Family,
	c ActionClass, level Level, why string, at time.Time) (State, bool, error) {
	why = truncate(why, 500)
	st, err := scanState(tx.QueryRow(ctx, `INSERT INTO sage.sre_family_autonomy
		(deployment_id, database_name, family, action_class, level, changed_by,
		 change_reason, changed_at, provenance, carried_ref)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'grandfathered', $9)
		ON CONFLICT (deployment_id, database_name, family, action_class) DO NOTHING
		RETURNING `+levelColumns, s.deployment, s.database, string(f), string(c),
		int16(level), ActorPgSage, "grandfathered from the time ramp", at, why))
	if errors.Is(err, pgx.ErrNoRows) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, storeErr("seed grandfathered level", err)
	}
	err = s.appendEvent(ctx, tx, Event{Family: f, Class: c, Type: EventGrandfathered,
		From: levelPtr(defaultLevel(f)), To: levelPtr(level), Actor: ActorPgSage,
		Reason: "grandfathered from the time ramp: " + why, At: at})
	return st, err == nil, err
}
