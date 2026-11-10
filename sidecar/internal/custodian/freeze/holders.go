package freeze

import (
	"fmt"
	"strings"
	"time"
)

// Kinds of xmin-horizon holders; the values are the sre/probes holder
// kinds of the shared xmin_horizon query (probes.XminHorizonSQL).
const (
	HolderSession      = "session"
	HolderStandby      = "standby"
	HolderPreparedXact = "prepared_xact"
	HolderSlot         = "slot"
	HolderSlotCatalog  = "slot_catalog"
)

// Holder shares of a table's XID age (the causal wraparound family uses
// the same PinShare and RefuteShare):
//   - a holder at least PinShare of the table age pins the table;
//   - under RefuteShare it cannot hold the table back at all;
//   - from FreezeStillHelpsShare (the mirror of RefuteShare) freezing
//     cannot advance relfrozenxid enough to lower the urgency.
const (
	PinShare              = 0.5
	RefuteShare           = 0.1
	FreezeStillHelpsShare = 0.9
)

// ResponseEscalateHolder is a plan that names a holder pg_sage never
// signals: a prepared transaction, a replication slot, a standby's
// feedback, or a protected session (a dump, a base backup, pg_sage).
const ResponseEscalateHolder ResponseKind = "escalate_xmin_holder"

// XminBlocker is one holder of the xmin horizon: a session (by PID and
// backend start), a standby (its walsender PID), a prepared transaction
// (Name is the md5 of its gid) or a replication slot (Name is the slot).
// XminAge is the older of its snapshot and XID ages. Protected sessions
// are never signalled. The backend ages are the session's own, when read.
type XminBlocker struct {
	Kind           string
	PID            int
	Name           string
	XminAge        int64
	User           string
	State          string
	Query          string
	AppName        string
	Protected      bool
	BackendStart   time.Time
	QueryStart     time.Time
	BackendXIDAge  *int64
	BackendXminAge *int64
}

// Pins reports whether a holder at holderAge pins a table at tableAge.
func Pins(holderAge, tableAge int64) bool {
	return tableAge > 0 && holderAge >= 0 &&
		float64(holderAge) >= PinShare*float64(tableAge)
}

// PinningCandidate is the oldest holder that applies to a table: a
// slot's catalog_xmin holds back only catalog tables (user_catalog_table).
func PinningCandidate(holders []XminBlocker, catalogTable bool) *XminBlocker {
	var oldest *XminBlocker
	for i := range holders {
		h := &holders[i]
		if h.Kind == HolderSlotCatalog && !catalogTable {
			continue
		}
		if oldest == nil || h.XminAge > oldest.XminAge {
			oldest = h
		}
	}
	return oldest
}

// HolderKey identifies a holder within one scan.
func HolderKey(h XminBlocker) string {
	return fmt.Sprintf("%s|%d|%s|%s", h.Kind, h.PID, h.Name,
		h.BackendStart.UTC().Format(time.RFC3339Nano))
}

func validateHolder(h XminBlocker) error {
	if h.XminAge < 0 {
		return fmt.Errorf("xmin holder %s age %d must not be negative", h.Kind, h.XminAge)
	}
	switch h.Kind {
	case HolderSession, HolderStandby:
		if h.PID <= 0 {
			return fmt.Errorf("xmin holder %s PID must be positive", h.Kind)
		}
	case HolderPreparedXact, HolderSlot, HolderSlotCatalog:
		if strings.TrimSpace(h.Name) == "" {
			return fmt.Errorf("xmin holder %s must be named", h.Kind)
		}
	default:
		return fmt.Errorf("unknown xmin holder kind %q", h.Kind)
	}
	return nil
}

// HolderResponse answers a holder that pins targets: an unprotected
// session is canceled (terminated when idle in a transaction, where a
// cancel ends nothing); every other holder is a parked escalation plan.
func HolderResponse(h XminBlocker, targets []string) (Response, error) {
	if len(targets) == 0 {
		return Response{}, fmt.Errorf("xmin holder %s pins no table", h.Kind)
	}
	if err := validateHolder(h); err != nil {
		return Response{}, err
	}
	evidence := holderEvidence(h, len(targets))
	if h.Kind != HolderSession || h.Protected {
		plan := escalationPlan(h)
		evidence["plan"] = plan
		return Response{Kind: ResponseEscalateHolder, Plan: plan, Evidence: evidence}, nil
	}
	kind, function := ResponseCancelBlocker, "pg_cancel_backend"
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(h.State)), "idle in transaction") {
		kind, function = ResponseTerminateBlocker, "pg_terminate_backend"
	}
	return Response{Kind: kind, SQL: fmt.Sprintf("SELECT %s(%d)", function, h.PID),
		Evidence: evidence}, nil
}

func escalationPlan(h XminBlocker) string {
	held := fmt.Sprintf("holds the xmin horizon at age %d", h.XminAge)
	switch h.Kind {
	case HolderPreparedXact:
		return fmt.Sprintf("Escalate: prepared transaction (gid md5 %s) %s; an operator "+
			"must commit or roll it back, pg_sage never resolves a prepared transaction",
			h.Name, held)
	case HolderSlot:
		return fmt.Sprintf("Escalate: replication slot %q %s through its xmin; confirm "+
			"the slot's consumer is alive before advancing or dropping it", h.Name, held)
	case HolderSlotCatalog:
		return fmt.Sprintf("Escalate: logical replication slot %q %s through its "+
			"catalog_xmin; confirm the slot's consumer is alive before advancing or "+
			"dropping it", h.Name, held)
	case HolderStandby:
		return fmt.Sprintf("Escalate: a standby's hot_standby_feedback (walsender pid %d) "+
			"%s; end the standby's long query or review its feedback", h.PID, held)
	}
	return fmt.Sprintf("Escalate: protected session pid %d (%s) %s; pg_sage never "+
		"signals it, let it finish or stop it by hand", h.PID, h.AppName, held)
}

// holderSummary names a holder in a freeze's evidence.
func holderSummary(h XminBlocker) map[string]any {
	return map[string]any{"kind": h.Kind, "pid": h.PID, "name": h.Name,
		"xmin_age": h.XminAge, "app_name": h.AppName, "protected": h.Protected}
}

func holderEvidence(h XminBlocker, pinned int) map[string]any {
	evidence := map[string]any{"holder_kind": h.Kind, "holder_name": h.Name, "pid": h.PID,
		"xmin_age": h.XminAge, "user": h.User, "state": h.State, "query": h.Query,
		"app_name": h.AppName, "protected": h.Protected, "pinned_tables": pinned,
		"backend_xid_age":  ageOrNil(h.BackendXIDAge),
		"backend_xmin_age": ageOrNil(h.BackendXminAge)}
	if !h.BackendStart.IsZero() {
		evidence["backend_start"] = h.BackendStart.UTC().Format(time.RFC3339Nano)
	}
	if !h.QueryStart.IsZero() {
		evidence["query_start"] = h.QueryStart.UTC().Format(time.RFC3339Nano)
	}
	return evidence
}

func ageOrNil(age *int64) any {
	if age == nil {
		return nil
	}
	return *age
}
