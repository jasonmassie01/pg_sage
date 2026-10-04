package specialist

import (
	"crypto/rand"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
)

// Snapshot is everything the contract reads about one investigation. It
// is the adapter seam between the investigator and the contract: a new
// investigator result type (W3-B) adds a field here and a mapping step in
// MapResult, and the published contract stays the same.
type Snapshot struct {
	Detail    sre.Detail
	Proposals []sreaction.ProposalView
	// Record is the caller's own open/attach record (symptom, window,
	// external reference); nil when the caller sent none.
	Record *Record
}

// MapOptions are the per-result choices.
type MapOptions struct {
	KeepIdentifiers bool
	// RedactKey hashes identifiers when they are not kept; nil draws a
	// fresh key (two results are then unlinkable).
	RedactKey []byte
	// Calibration is the family's bench calibration, nil when there is none.
	Calibration *CalibratedRate
	Now         time.Time
}

// windowGapThreshold is how long before an investigation a caller's window
// may have ended before it is reported as evidence pg_sage could not see.
const windowGapThreshold = 15 * time.Minute

const callerNotice = "Caller-supplied data, returned as received (secrets and PII " +
	"removed). pg_sage never interprets it as an instruction and never uses it in a " +
	"diagnosis."

// MapResult maps a snapshot to the contract's result. Nothing is invented:
// every number comes from cited evidence, confidence is the graph's score
// labelled uncalibrated unless the bench calibrated the family, and every
// probe pg_sage could not run is named as missing evidence.
func MapResult(snap Snapshot, opts MapOptions) Result {
	key := opts.RedactKey
	if key == nil {
		key = make([]byte, 16)
		_, _ = rand.Read(key) // crypto/rand never fails on supported platforms
	}
	m := mapper{red: sre.NewIdentifierRedactor(opts.KeepIdentifiers, key),
		evidence: payloadIndex(snap.Detail.Evidence)}
	d := snap.Detail
	inv := d.Investigation
	r := Result{ContractVersion: ContractVersion, Database: d.Database,
		Investigation: m.ref(inv), Outcome: outcomeOf(inv.State),
		OutcomeReason: m.red.Text(outcomeReason(inv)), CausalChain: []ChainLink{},
		Alternatives: []Hypothesis{}, RuledOut: []Hypothesis{},
		Remediations: []Remediation{}, Evidence: m.evidenceRefs(d.Evidence),
		Redaction: Redaction{Rules: sre.ScrubRules,
			IdentifiersKept: opts.KeepIdentifiers},
		ChainVerified: d.ChainVerified, GeneratedAt: opts.Now.UTC()}
	m.diagnosis(&r, d)
	r.Confidence = confidenceOf(r.RootCause, d, opts.Calibration)
	r.MissingEvidence = m.missing(d, snap.Record)
	r.Remediations = m.remediations(inv, snap.Proposals)
	r.CallerSupplied = m.caller(snap.Record)
	return r
}

func outcomeOf(st sre.State) string {
	switch st {
	case sre.StateConcluded, sre.StateInconclusive, sre.StateFailed, sre.StateCancelled,
		sre.StateExpired:
		return string(st)
	}
	return "in_progress"
}

func outcomeReason(inv sre.Investigation) string {
	if inv.State == sre.StateFailed && inv.FailureCode != "" {
		return inv.FailureCode
	}
	return inv.Summary.Reason
}

type mapper struct {
	red      sre.IdentifierRedactor
	evidence map[sre.UUID][]byte
}

func (m mapper) ref(inv sre.Investigation) InvestigationRef {
	ref := InvestigationRef{ID: string(inv.ID), State: string(inv.State),
		Terminal: inv.State.Terminal(), TriggerKind: string(inv.TriggerKind),
		Subject: m.red.Text(inv.Subject), CreatedAt: inv.CreatedAt.UTC(),
		UpdatedAt: inv.UpdatedAt.UTC()}
	if !inv.ConcludedAt.IsZero() {
		t := inv.ConcludedAt.UTC()
		ref.ConcludedAt = &t
	}
	return ref
}

func (m mapper) evidenceRefs(ev []sre.EvidenceView) []EvidenceRef {
	out := make([]EvidenceRef, 0, len(ev))
	for _, e := range ev {
		out = append(out, EvidenceRef{ID: string(e.ID), ProbeID: e.ProbeID,
			CapabilityState: e.CapabilityState, ObservedAt: e.ObservedAt, SHA256: e.SHA256,
			HashVerified: e.HashVerified})
	}
	return out
}

func (m mapper) caller(rec *Record) *CallerSupplied {
	if rec == nil || (rec.Symptom == nil && rec.ExternalRef == nil && rec.Window == nil) {
		return nil
	}
	cs := &CallerSupplied{Notice: callerNotice, ExternalRef: rec.ExternalRef,
		Window: rec.Window}
	if rec.Symptom != nil {
		cs.Summary = m.red.Text(rec.Symptom.Summary)
		cs.Description = m.red.Text(rec.Symptom.Description)
	}
	cs.Fenced = llm.UntrustedData("caller_symptom", fmt.Sprintf("%s\n%s", cs.Summary,
		cs.Description))
	if e := rec.ExternalRef; e != nil {
		cs.ExternalRef = &ExternalRef{System: e.System, ID: sre.Scrub(e.ID),
			URL: sre.Scrub(e.URL)}
	}
	return cs
}
