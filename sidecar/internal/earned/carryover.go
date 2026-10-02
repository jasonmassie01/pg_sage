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

// Carry-over (coordinator decision 2026-10-02): M7 gates NEW autonomy. A
// pair pg_sage already ran autonomously under decided policy keeps the
// level today's configuration grants it, with provenance carried_over and
// the decision behind it. It is granted by policy, not earned by
// evidence: it does not decay with evidence, downgrade signals cap it
// while they hold, and it returns by itself when they clear.

// CarryOver is one pair that was autonomous before the ledger existed.
type CarryOver struct {
	Family Family
	Class  ActionClass
	// Tier is the contract risk tier the pre-M7 gate authorized it under.
	Tier policy.RiskTier
	// Ref is the decision that granted the autonomy.
	Ref string
}

var carryOvers = []CarryOver{
	{FamilyWraparound, ClassFreeze, policy.RiskSafe, "spec F3 " +
		"(specs/agent-native-autonomy-build-spec.md): the wraparound custodian freezes " +
		"autonomously, waiting risks an outage; deadline override per §1.7"},
	{FamilyWraparound, ClassAutovacuumTuning, policy.RiskModerate, "spec F3 " +
		"(specs/agent-native-autonomy-build-spec.md): autonomous autovacuum tuning; " +
		"decision D1 made set_table_autovacuum reversible"},
	{FamilyWAL, ClassWALBound, policy.RiskModerate, "spec F4 " +
		"(specs/agent-native-autonomy-build-spec.md): the WAL custodian bounds " +
		"max_slot_wal_keep_size, never below retained WAL plus headroom (P0-07)"},
	{FamilyPlanRegression, ClassIndexCreate, policy.RiskModerate, "decision D6 " +
		"(reviews/decisions/LEDGER.md): load-admitted autonomous index creation"},
}

// CarryOvers lists the pairs that were autonomous before M7.
func CarryOvers() []CarryOver { return append([]CarryOver(nil), carryOvers...) }

func carryOverFor(f Family, c ActionClass) (CarryOver, bool) {
	for _, co := range carryOvers {
		if co.Family == f && co.Class == c {
			return co, true
		}
	}
	return CarryOver{}, false
}

// carryCap is the highest level a carry-over keeps: irreversible classes
// never exceed L1, L4 is never reached.
func carryCap(c ActionClass) Level {
	if spec, ok := Spec(c); !ok || spec.Reversibility == Irreversible {
		return L1
	}
	return L3
}

// effectiveCap is the cap the gate applies to a pair: a carried level
// keeps the autonomy policy granted (up to carryCap), an earned one the
// promotion cap.
func effectiveCap(st State, c ActionClass) Level {
	if st.Provenance == ProvenanceCarriedOver {
		return carryCap(c)
	}
	return CapFor(c)
}

// CarriedLevel is the level the operator's bound lets the pre-M7 gate run
// the pair at autonomously: L3 when the configuration executes its tier
// unattended, else L1 (it was not autonomous).
func CarriedLevel(bound policy.RuntimeState, f Family, c ActionClass) Level {
	co, ok := carryOverFor(f, c)
	if !ok || !autonomousUnder(bound, co.Tier) {
		return L1
	}
	return MinLevel(L3, carryCap(c))
}

// autonomousUnder mirrors the gate's tier decision without the trust ramp
// clock (the gate still enforces the ramp on every action).
func autonomousUnder(b policy.RuntimeState, tier policy.RiskTier) bool {
	if !b.ExecutorEnabled || b.ExecutionMode != policy.ExecutionAuto {
		return false
	}
	switch tier {
	case policy.RiskSafe:
		return b.Tier3Safe &&
			(b.TrustLevel == policy.TrustAdvisory || b.TrustLevel == policy.TrustAutonomous)
	case policy.RiskModerate:
		return b.Tier3Moderate && b.TrustLevel == policy.TrustAutonomous
	}
	return false
}

// SeedCarriedOver seeds every pair the database's bound lets run
// autonomously and that has no ledger row yet in that database's ledger
// (carry-over is per database, P0-5). It never raises or lowers an
// existing row, so it is idempotent. It returns the pairs seeded now.
func (s *Service) SeedCarriedOver(ctx context.Context, database string,
	bound policy.RuntimeState) ([]State, error) {
	if err := s.store.checkDatabase(database); err != nil {
		return nil, err
	}
	seeded := []State{}
	for _, co := range carryOvers {
		level := CarriedLevel(bound, co.Family, co.Class)
		if level <= defaultLevel(co.Family) {
			continue
		}
		st, ok, err := s.store.seedCarried(ctx, co, level, bound, s.now())
		if err != nil {
			return seeded, err
		}
		if ok {
			seeded = append(seeded, st)
		}
	}
	s.invalidate()
	return seeded, nil
}

// seedCarried inserts a carried-over row and its history entry when the
// pair has no row; ok is false when it already had one.
func (s *PostgresStore) seedCarried(ctx context.Context, co CarryOver, level Level,
	bound policy.RuntimeState, at time.Time) (State, bool, error) {
	evidence, err := json.Marshal(map[string]any{"tier": co.Tier,
		"trust_level": bound.TrustLevel, "execution_mode": bound.ExecutionMode,
		"tier3_safe": bound.Tier3Safe, "tier3_moderate": bound.Tier3Moderate})
	if err != nil {
		return State{}, false, fmt.Errorf("encode carry-over evidence: %w", err)
	}
	var st State
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		st, err = scanState(tx.QueryRow(ctx, `INSERT INTO sage.sre_family_autonomy
			(deployment_id, database_name, family, action_class, level, evidence,
			 changed_by, change_reason, changed_at, provenance, carried_ref)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'carried_over', $10)
			ON CONFLICT (deployment_id, database_name, family, action_class) DO NOTHING
			RETURNING `+levelColumns, s.deployment, s.database, string(co.Family),
			string(co.Class), int16(level), evidence, ActorPgSage,
			"carried over from the pre-M7 policy", at, co.Ref))
		if err != nil {
			return err
		}
		return s.appendEvent(ctx, tx, Event{Family: co.Family, Class: co.Class,
			Type: EventCarriedOver, From: levelPtr(defaultLevel(co.Family)),
			To: levelPtr(level), Actor: ActorPgSage,
			Reason: "carried over from the pre-M7 policy: " + co.Ref,
			Evidence: evidence, At: at})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return State{}, false, nil
	}
	return st, err == nil, storeErr("seed carried-over level", err)
}
