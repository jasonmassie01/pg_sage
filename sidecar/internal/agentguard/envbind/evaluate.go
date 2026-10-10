package envbind

import (
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// Reason codes: why a database evaluates as it does.
const (
	ReasonDefaultProd        = "default_prod"          // no label: prod
	ReasonLabelProd          = "label_prod"            // labelled prod
	ReasonVerified           = "verified"              // non-prod label, binding verified
	ReasonLabelUnverified    = "label_unverified"      // the label was stored unverified
	ReasonBindingChanged     = "binding_changed"       // the live tuple differs (re-pointed DSN)
	ReasonUnverifiable       = "identity_unverifiable" // only host:port is known
	ReasonReceiptMissing     = "receipt_missing"       // branch without an active receipt
	ReasonReceiptMismatch    = "receipt_mismatch"      // receipt names another resource
	ReasonTwoLabels          = "two_labels"            // one physical database, two labels
	ReasonNoControl          = "no_control_database"   // governance runs posture-only
	ReasonUnbound            = "unbound"               // no database_id yet
	ReasonIdentityUnreadable = "identity_unreadable"   // the live tuple could not be read
)

// Errors. Each is distinguishable with errors.Is.
var (
	ErrLabelRefused    = errors.New("envbind: environment label refused")
	ErrNoControl       = errors.New("envbind: agent governance needs mode: meta or agents.control_database")
	ErrUnbound         = errors.New("envbind: the database has no database_id yet")
	ErrInvalidActor    = errors.New("envbind: a label change names who made it")
	ErrUnknownDatabase = errors.New("envbind: unknown database")
	ErrFenced          = errors.New("envbind: the leader lease moved; write fenced off")
)

// RefusedError is a label that cannot be verified, with the reason code
// and the one fix that would allow it.
type RefusedError struct {
	Reason string
	Detail string
	Fix    string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("%v: %s: %s", ErrLabelRefused, e.Reason, e.Detail)
}

// Unwrap makes errors.Is(err, ErrLabelRefused) hold.
func (e *RefusedError) Unwrap() error { return ErrLabelRefused }

// LabelRecord is a stored label row.
type LabelRecord struct {
	DatabaseID   string     `json:"database_id"`
	Label        Env        `json:"label"`
	Identity     Identity   `json:"identity"`
	Verified     bool       `json:"verified"`
	SetBy        string     `json:"set_by"`
	SetAt        time.Time  `json:"set_at"`
	Observed     *Identity  `json:"observed,omitempty"`
	ObservedAt   *time.Time `json:"observed_at,omitempty"`
	PendingLabel Env        `json:"pending_label,omitempty"`
	PendingBy    string     `json:"pending_by,omitempty"`
	PendingAt    *time.Time `json:"pending_at,omitempty"`
}

// Peer is another database's label and last observed identity.
type Peer struct {
	DatabaseID string   `json:"database_id"`
	Label      Env      `json:"label"`
	Identity   Identity `json:"identity"`
}

// Evidence is why a database evaluates as its environment.
type Evidence struct {
	DatabaseID string         `json:"database_id"`
	Database   string         `json:"database"`
	Label      Env            `json:"label"`
	Effective  Env            `json:"effective"`
	Verified   bool           `json:"verified"`
	Reasons    []string       `json:"reasons"`
	Critical   bool           `json:"critical"`
	Strength   Strength       `json:"strength"`
	Live       Identity       `json:"live"`
	Snapshot   *Identity      `json:"snapshot,omitempty"`
	Changed    []string       `json:"changed,omitempty"`
	Conflicts  []Peer         `json:"conflicts,omitempty"`
	Receipt    *clone.Receipt `json:"receipt,omitempty"`
	SetBy      string         `json:"set_by,omitempty"`
	SetAt      *time.Time     `json:"set_at,omitempty"`
}

// Binding is a database's environment and the evidence behind it.
type Binding struct {
	Env      Env      `json:"env"`
	Evidence Evidence `json:"evidence"`
}

type evalInput struct {
	Row     *LabelRecord
	Live    Identity
	Peers   []Peer
	Receipt *clone.Receipt
}

// evaluate decides a database's environment from its stored label, its
// live identity, the other databases' identities and its clone receipt.
// Every failed check leaves it prod.
func evaluate(in evalInput) Evidence {
	ev := Evidence{Label: EnvProd, Effective: EnvProd, Live: in.Live,
		Strength: in.Live.Strength(), Receipt: in.Receipt}
	if in.Row == nil {
		ev.Reasons = []string{ReasonDefaultProd}
		ev.Conflicts = conflicts(EnvProd, in.Live, in.Peers)
		ev.Critical = len(ev.Conflicts) > 0
		return ev
	}
	snap := in.Row.Identity
	ev.Label, ev.Snapshot, ev.SetBy = in.Row.Label, &snap, in.Row.SetBy
	setAt := in.Row.SetAt
	ev.SetAt = &setAt
	ev.Conflicts = conflicts(in.Row.Label, in.Live, in.Peers)
	if len(ev.Conflicts) > 0 {
		ev.Reasons, ev.Critical = append(ev.Reasons, ReasonTwoLabels), true
	}
	if in.Row.Label == EnvProd {
		ev.Reasons = append([]string{ReasonLabelProd}, ev.Reasons...)
		return ev
	}
	checkNonProd(&ev, in)
	if len(ev.Reasons) == 0 {
		ev.Effective, ev.Verified, ev.Reasons = in.Row.Label, true, []string{ReasonVerified}
	}
	return ev
}

// checkNonProd adds every reason a non-prod label does not hold.
func checkNonProd(ev *Evidence, in evalInput) {
	if !in.Row.Verified {
		ev.Reasons = append(ev.Reasons, ReasonLabelUnverified)
	}
	if ev.Changed = in.Row.Identity.Diff(in.Live); len(ev.Changed) > 0 {
		ev.Reasons, ev.Critical = append(ev.Reasons, ReasonBindingChanged), true
	}
	if in.Live.Strength() == StrengthConfigured {
		ev.Reasons = append(ev.Reasons, ReasonUnverifiable)
	}
	if in.Row.Label != EnvBranch {
		return
	}
	if reason := receiptReason(in.Live, in.Receipt); reason != "" {
		ev.Reasons, ev.Critical = append(ev.Reasons, reason), true
	}
}

// receiptReason checks a branch's receipt against the live identity.
func receiptReason(live Identity, r *clone.Receipt) string {
	if r == nil || r.ProviderRef() == "" {
		return ReasonReceiptMissing
	}
	if live.ProviderRef != r.ProviderRef() {
		return ReasonReceiptMismatch
	}
	return ""
}

// conflicts lists the peers with the same physical identity under another
// label that no provider resource id tells apart.
func conflicts(label Env, live Identity, peers []Peer) []Peer {
	key := live.PhysicalKey()
	if key == "" {
		return nil
	}
	var out []Peer
	for _, p := range peers {
		if p.Identity.PhysicalKey() == key && p.Label != label &&
			!distinguished(live, p.Identity) {
			out = append(out, p)
		}
	}
	return out
}

// checkLabel refuses a label the live identity cannot verify.
func checkLabel(label Env, live Identity, peers []Peer, r *clone.Receipt) error {
	if label == EnvProd {
		return nil
	}
	if live.Strength() == StrengthConfigured {
		return &RefusedError{Reason: ReasonUnverifiable,
			Detail: "only the connection target is known; a non-prod label needs the " +
				"system identifier or a provider resource id",
			Fix: "GRANT EXECUTE ON FUNCTION pg_catalog.pg_control_system() TO <pg_sage role>"}
	}
	if label == EnvBranch {
		if reason := receiptReason(live, r); reason != "" {
			return &RefusedError{Reason: reason,
				Detail: "a branch label needs an active clone receipt naming this database",
				Fix:    "create the branch through pg_sage so it has a receipt, or label it dev"}
		}
	}
	if c := conflicts(label, live, peers); len(c) > 0 {
		return &RefusedError{Reason: ReasonTwoLabels,
			Detail: fmt.Sprintf("database %s has the same physical identity (%s) as %s, "+
				"labelled %s", live.Target, live.PhysicalKey(), c[0].DatabaseID, c[0].Label),
			Fix: "this is a copy, standby or re-pointed DSN of that database; give it its " +
				"own provider resource id or keep it prod"}
	}
	return nil
}
