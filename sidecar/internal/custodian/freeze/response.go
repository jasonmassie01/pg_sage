package freeze

import (
	"fmt"
	"strings"
)

type ResponseKind string

const (
	ResponseCancelBlocker    ResponseKind = "cancel_xmin_blocker"
	ResponseTerminateBlocker ResponseKind = "terminate_xmin_blocker"
	ResponseVacuumFreeze     ResponseKind = "vacuum_freeze"
	ResponsePlanRepack       ResponseKind = "plan_pg_repack"
	ResponseParkBloat        ResponseKind = "park_bloat"
)

type ResponseInput struct {
	Schema  string
	Table   string
	Urgency Urgency
	// XIDAge is the table's age(relfrozenxid); Holder is the oldest xmin
	// holder that applies to the table (PinningCandidate). A red table
	// attaches the holder only when it pins the table (Pins).
	XIDAge int64
	Holder *XminBlocker
	// MultiXactAge is mxid_age(relminmxid); with Aggressive it picks a plain
	// VACUUM over VACUUM (FREEZE) (VacuumSQL).
	MultiXactAge   int64
	Aggressive     AggressiveAges
	DeadTupleRatio float64
	// BloatRatio is the heap's bloat estimate (or its pgstattuple
	// measurement, BloatSource); it counts only when BloatKnown. The
	// dead-tuple ratio is never bloat: the analyzer's VACUUM owns it.
	BloatRatio  float64
	BloatKnown  bool
	BloatSource string
	// SizeBytes is the heap size; a bloat plan needs at least
	// MinBloatBytes, and a zero floor plans nothing (fail closed).
	SizeBytes         int64
	MinBloatBytes     int64
	PGRepackAvailable bool
	// CapFailures counts the table's custodian VACUUMs the watchdog capped
	// in the backoff window; from EmergencyCapFailures a red table gets
	// the emergency profile (EmergencyVacuumSQL).
	CapFailures int
}

type Response struct {
	Kind     ResponseKind
	SQL      string
	Plan     string
	Evidence map[string]any
	// PinnedBy is the holder that pins a red table: it gets one holder
	// proposal per scan (HolderResponse), whether or not the table is
	// still frozen.
	PinnedBy *XminBlocker
}

// PlanResponse answers one table: a red table is frozen (or its pinning
// holder named), and a known bloat estimate is planned. Amber gets nothing
// from the custodian: autovacuum tuning belongs to the analyzer alone,
// which reads the table's reloptions and restores them on rollback
// (vacuum-07).
func PlanResponse(input ResponseInput) (Response, error) {
	if err := validateResponseInput(input); err != nil {
		return Response{}, err
	}
	if input.Urgency == UrgencyRed {
		return freezeResponse(input), nil
	}
	if bloatPlanned(input) {
		return bloatResponse(input), nil
	}
	return Response{}, nil
}

// bloatPlanned reports a known bloat estimate of at least
// BloatPlanThreshold on a heap of at least the configured floor.
func bloatPlanned(input ResponseInput) bool {
	return input.BloatKnown && input.BloatRatio >= BloatPlanThreshold &&
		input.MinBloatBytes > 0 && input.SizeBytes >= input.MinBloatBytes
}

func validateResponseInput(input ResponseInput) error {
	if strings.TrimSpace(input.Schema) == "" || strings.TrimSpace(input.Table) == "" {
		return fmt.Errorf("schema-qualified freeze target is required")
	}
	if input.XIDAge < 0 {
		return fmt.Errorf("table XID age %d must not be negative", input.XIDAge)
	}
	if input.MultiXactAge < 0 {
		return fmt.Errorf("table multixact age %d must not be negative", input.MultiXactAge)
	}
	if input.Holder != nil {
		if err := validateHolder(*input.Holder); err != nil {
			return err
		}
	}
	if input.DeadTupleRatio < 0 || input.DeadTupleRatio > 1 ||
		input.BloatRatio < 0 || input.BloatRatio > 1 {
		return fmt.Errorf("freeze ratios must be within zero and one")
	}
	if input.CapFailures < 0 {
		return fmt.Errorf("cap failure count %d must not be negative", input.CapFailures)
	}
	if input.SizeBytes < 0 || input.MinBloatBytes < 0 {
		return fmt.Errorf("heap size %d and bloat floor %d must not be negative",
			input.SizeBytes, input.MinBloatBytes)
	}
	return nil
}

// freezeResponse freezes a red table unless its pinning holder is so old
// that freezing cannot lower its urgency (at least FreezeStillHelpsShare
// of the table age): the VACUUM still advances relfrozenxid up to the
// holder's horizon.
func freezeResponse(input ResponseInput) Response {
	holder := input.Holder
	if holder == nil || !Pins(holder.XminAge, input.XIDAge) {
		return Response{Kind: ResponseVacuumFreeze, SQL: freezeSQL(input),
			Evidence: responseEvidence(input)}
	}
	pinned := *holder
	if float64(holder.XminAge) >= FreezeStillHelpsShare*float64(input.XIDAge) {
		return Response{PinnedBy: &pinned}
	}
	evidence := responseEvidence(input)
	evidence["xmin_holder"] = holderSummary(pinned)
	return Response{Kind: ResponseVacuumFreeze, SQL: freezeSQL(input),
		Evidence: evidence, PinnedBy: &pinned}
}

func freezeSQL(input ResponseInput) string {
	if emergencyProfile(input) {
		return EmergencyVacuumSQL(input.Schema, input.Table, input.XIDAge, input.MultiXactAge,
			input.Aggressive)
	}
	return VacuumSQL(input.Schema, input.Table, input.XIDAge, input.MultiXactAge,
		input.Aggressive)
}

func bloatResponse(input ResponseInput) Response {
	response := Response{Evidence: responseEvidence(input)}
	if input.PGRepackAvailable {
		response.Kind = ResponsePlanRepack
		response.Plan = "Plan pg_repack --table=" + input.Schema + "." + input.Table +
			" after disk and lock-window validation"
		return response
	}
	response.Kind = ResponseParkBloat
	response.Plan = "Park bloat remediation until an online rebuild provider is available"
	return response
}

func responseEvidence(input ResponseInput) map[string]any {
	evidence := map[string]any{"dead_tuple_ratio": input.DeadTupleRatio,
		"pg_repack_available": input.PGRepackAvailable, "heap_bytes": input.SizeBytes,
		"bloat_min_bytes": input.MinBloatBytes, "bloat_threshold": BloatPlanThreshold}
	if input.BloatKnown {
		evidence["bloat_ratio"] = input.BloatRatio
		evidence["bloat_source"] = input.BloatSource
	}
	if emergencyProfile(input) {
		evidence["profile"] = ProfileEmergency
		evidence["cap_failures"] = input.CapFailures
	}
	return evidence
}
