package earned

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// EventEvidenceExpiring records the one warning that a pair's earned
// level holds only until its evidence expires (earned-06).
const EventEvidenceExpiring EventType = "evidence_expiring"

// SweepResult counts one evidence sweep.
type SweepResult struct {
	// Pairs are the earned incident pairs at L2 or more that were swept;
	// SelfPairs the self-initiated L3 pairs whose major was re-checked
	// (earned-07). Capped and Cleared count both.
	Pairs     int `json:"pairs"`
	SelfPairs int `json:"self_pairs"`
	Capped    int `json:"capped"`
	Cleared   int `json:"cleared"`
	Warned    int `json:"warned"`
}

// ExpiryWarning says an earned level holds only until HoldsUntil unless
// new evidence (Checks name what expires) arrives.
type ExpiryWarning struct {
	Database   string      `json:"database"`
	Family     Family      `json:"family"`
	Class      ActionClass `json:"class"`
	Level      Level       `json:"level"`
	HoldsUntil time.Time   `json:"holds_until"`
	Checks     []string    `json:"checks"`
	At         time.Time   `json:"at"`
}

// ExpiryNotifier tells an operator about an expiring earned level.
type ExpiryNotifier interface {
	NotifyEvidenceExpiring(ctx context.Context, w ExpiryWarning) error
}

// Sweep re-reads the evidence of every earned incident pair at L2 or
// more on this limiter's database, without waiting for an action: it
// records a decay as a cap (and its clearing), keeping the recorded
// downgrade signals, and warns once, ExpiryWarningLead ahead, before a
// level's evidence expires. Carried-over and self-initiated levels do not
// decay; a self-initiated L3 has its PostgreSQL major re-checked instead
// (earned-07). A pair's failure does not stop the others; the
// failures are returned joined. n may be nil (recorded only).
func (l *Limiter) Sweep(ctx context.Context, n ExpiryNotifier) (SweepResult, error) {
	var res SweepResult
	if err := l.svc.store.checkDatabase(l.b.Database); err != nil {
		return res, fmt.Errorf("limiter bound to another ledger: %w", err)
	}
	stored, err := l.svc.store.Levels(ctx)
	if err != nil {
		return res, err
	}
	var errs []error
	for _, row := range stored {
		if reValidates(row) {
			res.SelfPairs++
			l.sweepMajor(ctx, row, &res)
			continue
		}
		if !decays(row) {
			continue
		}
		st, err := l.svc.Granted(ctx, row.Family, row.Class)
		if err != nil {
			return res, err
		}
		if !decays(st) {
			continue
		}
		res.Pairs++
		if err := l.sweepPair(ctx, st, n, &res); err != nil {
			errs = append(errs, fmt.Errorf("sweep %s/%s: %w", st.Family, st.Class, err))
		}
	}
	return res, errors.Join(errs...)
}

// decays reports a level that its evidence must keep supporting.
func decays(st State) bool {
	return st.Level >= L2 && st.Provenance != ProvenanceCarriedOver &&
		!IsSelfInitiated(st.Family)
}

func (l *Limiter) sweepPair(ctx context.Context, st State, n ExpiryNotifier,
	res *SweepResult) error {
	p := &pairEvidence{svc: l.svc, f: st.Family, c: st.Class}
	supported, ev, err := p.get(ctx)
	if err != nil {
		return err
	}
	decay := decayOf(l.svc.cfg.Thresholds, st, supported, ev)
	typ, _ := l.noteTransition(ctx, st.Family, st.Class, func(prev capState) capState {
		return capState{downs: prev.downs, decay: decay}
	})
	switch typ {
	case EventCapped:
		res.Capped++
	case EventCapCleared:
		res.Cleared++
	}
	if decay != nil {
		return nil
	}
	at, checks, err := l.svc.expiryAt(ctx, ev)
	if err != nil || at.IsZero() || at.Sub(l.svc.now()) > ExpiryWarningLead {
		return err
	}
	w := ExpiryWarning{Database: l.b.Database, Family: st.Family, Class: st.Class,
		Level: st.Level, HoldsUntil: at, Checks: checks, At: l.svc.now()}
	warned, err := l.recordWarning(ctx, w)
	if err != nil || !warned {
		return err
	}
	res.Warned++
	if n == nil {
		return nil
	}
	if err := n.NotifyEvidenceExpiring(ctx, w); err != nil {
		return fmt.Errorf("notify evidence expiry (recorded, not retold): %w", err)
	}
	return nil
}

// expiryEvidence is what a warning records; HoldsUntil keys it, so one
// expiry is warned about once (sameWarning).
type expiryEvidence struct {
	HoldsUntil time.Time `json:"holds_until"`
	Checks     []string  `json:"checks"`
	Granted    Level     `json:"granted"`
}

// sameWarning reports whether a warning already sent for warned tells an
// expiry at next too: the same expiry, or one that moved later by at most
// ExpiryWarningLead. A shadow window renewed review by review, or a report
// only a little newer, moves the date on every sweep; telling each move
// would repeat one lapse. An earlier expiry, or one renewed past the lead,
// is new.
func sameWarning(warned, next time.Time) bool {
	return !next.Before(warned) && next.Sub(warned) <= ExpiryWarningLead
}

// recordWarning records w unless the pair's newest warning already tells
// it (sameWarning); false when it does.
func (l *Limiter) recordWarning(ctx context.Context, w ExpiryWarning) (bool, error) {
	key := pairKey{w.Family, w.Class}
	l.noteMu.Lock()
	defer l.noteMu.Unlock()
	if last, ok := l.warned[key]; ok && sameWarning(last, w.HoldsUntil) {
		return false, nil
	}
	stored, err := l.svc.store.latestEvent(ctx, w.Family, w.Class, EventEvidenceExpiring)
	if err != nil {
		return false, err
	}
	if stored != nil {
		var prev expiryEvidence
		if json.Unmarshal(stored.Evidence, &prev) == nil &&
			sameWarning(prev.HoldsUntil, w.HoldsUntil) {
			l.warned[key] = prev.HoldsUntil
			return false, nil
		}
	}
	raw, err := json.Marshal(expiryEvidence{HoldsUntil: w.HoldsUntil, Checks: w.Checks,
		Granted: w.Level})
	if err != nil { // plain fields: cannot fail
		raw = []byte(`{}`)
	}
	e := Event{Family: w.Family, Class: w.Class, Type: EventEvidenceExpiring,
		Actor: ActorPgSage, Reason: ExpiryText(w.Level, w.HoldsUntil, w.Checks),
		Evidence: raw, At: w.At}
	if err := l.svc.store.appendEvent(ctx, l.svc.store.pool, e); err != nil {
		return false, err
	}
	l.warned[key] = w.HoldsUntil
	return true, nil
}

// ExpiryText says, for an operator, until when level holds and what
// would renew it.
func ExpiryText(level Level, until time.Time, checks []string) string {
	var renew []string
	bench, shadow := false, false
	for _, c := range checks {
		switch {
		case c == "bench_fresh" && !bench:
			bench = true
			renew = append(renew, "a new bench report arrives")
		case strings.HasPrefix(c, "shadow_") && !shadow:
			shadow = true
			renew = append(renew, "more packets are reviewed by a person")
		}
	}
	if len(renew) == 0 {
		renew = []string{"new evidence arrives"}
	}
	return fmt.Sprintf("%s holds until %s unless %s", level,
		until.UTC().Format("2006-01-02 15:04 UTC"), strings.Join(renew, " or "))
}
