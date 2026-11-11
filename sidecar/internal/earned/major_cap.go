package earned

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// earned-07: a self-initiated level changes only by an approved promotion
// or a demerit, so nothing re-validated an unattended L3 after a
// PostgreSQL major upgrade, where planner, statistics and GUC semantics
// differ. Every level write now records the server's major; a
// self-initiated L3 granted on another major than the server runs now is
// capped at L2 (recorded as a cap) until ClassMinSuccessesL2 improved
// outcomes recorded after the cap event, or an admin's re-approval
// (Revalidate), re-validate it on the new major; either restamps the row.
// A row from before earned-07 (no major) is stamped without a cap. An
// unreadable server version neither caps nor stamps. Incident families
// keep their evidence rule, carried-over levels their policy grant, and
// the demerit path is unchanged. An index replacement's own cap is L2, so
// a composite never needs it.

// ErrNotCapped is a re-approval of a level no major change caps.
var ErrNotCapped = errors.New("the level is not capped by a PostgreSQL major change")

// majorNote is a self-initiated L3 granted on From, observed on To.
type majorNote struct {
	From int `json:"from"`
	To   int `json:"to"`
}

// majorReasonPrefix starts a major cap's reason.
const majorReasonPrefix = "server_major_changed:"

func (m *majorNote) reason() string {
	return fmt.Sprintf("%s%d->%d", majorReasonPrefix, m.From, m.To)
}

// clearedText is the cap_cleared reason; why defaults to "re-validated".
func (m *majorNote) clearedText(why string) string {
	if why == "" {
		why = "re-validated"
	}
	return fmt.Sprintf("PostgreSQL %d→%d %s", m.From, m.To, why)
}

// majorCheck is a pair's major cap now: note nil when nothing caps it;
// known false when the server major could not be read (keep the
// recorded cap); why says how a cap ended.
type majorCheck struct {
	note  *majorNote
	known bool
	why   string
}

// MajorCap is a Trust or Autonomy row's major cap.
type MajorCap struct {
	From     int    `json:"from"`
	To       int    `json:"to"`
	Improved int    `json:"improved"`
	Needed   int    `json:"needed"`
	Text     string `json:"text"`
}

// MajorCapText says a major cap and what lifts it, for an operator.
func MajorCapText(from, to, improved, needed int) string {
	return fmt.Sprintf("capped at L2: PostgreSQL %d→%d; %d of %d improved outcomes, "+
		"or re-approve", from, to, improved, needed)
}

// reValidates reports a level that must hold on the server's major.
func reValidates(st State) bool {
	return IsSelfInitiated(st.Family) && st.Level >= L3 &&
		st.Provenance != ProvenanceCarriedOver
}

// majorCap checks st's recorded major against the server's: a row
// without one is stamped (no cap); a different one caps until enough
// improved outcomes restamp it. Failures are logged; a failed stamp is
// retried by the next call.
func (l *Limiter) majorCap(ctx context.Context, st State) majorCheck {
	if !reValidates(st) {
		return majorCheck{known: true, why: "cap ended: the level is below L3"}
	}
	if l.b.ServerMajor == nil {
		return majorCheck{known: true}
	}
	current, err := l.b.ServerMajor(ctx)
	if err != nil || current <= 0 {
		return majorCheck{} // the probe logged it
	}
	if st.ServerMajor == nil {
		if _, err := l.stampMajor(ctx, st, nil, current); err != nil {
			l.svc.cfg.logf("autonomy: stamp PostgreSQL %d on %s/%s of %s: %v", current,
				st.Family, st.Class, l.b.Database, err)
		}
		return majorCheck{known: true}
	}
	if *st.ServerMajor == current {
		// Only a recorded cap reads why: the server went back to the major
		// the level was granted on.
		return majorCheck{known: true,
			why: fmt.Sprintf("cap ended: the server runs PostgreSQL %d again", current)}
	}
	capped := majorCheck{note: &majorNote{From: *st.ServerMajor, To: current}, known: true}
	n, err := l.svc.store.improvedSinceMajorCap(ctx, st.Family, st.Class, current)
	if err != nil {
		l.svc.cfg.logf("autonomy: count improved outcomes of %s/%s on %s: %v", st.Family,
			st.Class, l.b.Database, err)
		return capped
	}
	if n < l.svc.cfg.Thresholds.ClassMinSuccessesL2 {
		return capped
	}
	ok, err := l.stampMajor(ctx, st, st.ServerMajor, current)
	if err != nil || !ok {
		if err != nil {
			l.svc.cfg.logf("autonomy: restamp %s/%s of %s on PostgreSQL %d: %v", st.Family,
				st.Class, l.b.Database, current, err)
		}
		return capped
	}
	return majorCheck{known: true,
		why: fmt.Sprintf("re-validated by %d improved outcomes on PostgreSQL %d", n, current)}
}

// stampMajor records to as st's major if the row is still at st's
// version and major want (nil: none); false when it changed.
func (l *Limiter) stampMajor(ctx context.Context, st State, want *int, to int) (bool,
	error) {
	return l.svc.store.stampMajor(ctx, l.svc.store.pool, st, want, to)
}

// sweepMajor re-checks a self-initiated L3 pair's major for Sweep.
func (l *Limiter) sweepMajor(ctx context.Context, st State, res *SweepResult) {
	m := l.majorCap(ctx, st)
	if !m.known {
		return
	}
	typ, _ := l.noteTransition(ctx, st.Family, st.Class, func(prev capState) capState {
		return capState{downs: prev.downs, decay: prev.decay, major: m.note, why: m.why}
	})
	switch typ {
	case EventCapped:
		res.Capped++
	case EventCapCleared:
		res.Cleared++
	}
}

// Revalidate is an admin's re-approval of a self-initiated level capped
// by a PostgreSQL major change: it restamps the server's major and
// records the cap's clearing with actor and note, in one transaction.
func (l *Limiter) Revalidate(ctx context.Context, f Family, c ActionClass, actor,
	note string) (State, error) {
	if err := l.svc.store.checkDatabase(l.b.Database); err != nil {
		return State{}, err
	}
	actor = strings.TrimSpace(actor)
	if actor == "" || actor == ActorPgSage {
		return State{}, fmt.Errorf("%w: a re-approval names its admin", ErrHumanApprovalRequired)
	}
	if !IsSelfInitiated(f) || SelfFamilyFor(c) != f {
		return State{}, fmt.Errorf("%w: %s/%s is not a self-initiated pair", ErrInvalidRequest,
			f, c)
	}
	current, err := l.serverMajor(ctx)
	if err != nil {
		return State{}, err
	}
	st, found, err := l.svc.store.readLevel(ctx, l.svc.store.pool, f, c)
	if err != nil {
		return State{}, err
	}
	if !found || !reValidates(st) || st.ServerMajor == nil || *st.ServerMajor == current {
		return State{}, fmt.Errorf("%w: %s/%s on PostgreSQL %d", ErrNotCapped, f, c, current)
	}
	why := "re-validated: re-approved by " + actor
	if note = strings.TrimSpace(note); note != "" {
		why += ": " + truncate(note, 500)
	}
	prev := capState{major: &majorNote{From: *st.ServerMajor, To: current}}
	e := capEvent(f, c, prev, capState{by: actor, why: why}, l.svc.now())
	if err := l.svc.store.revalidate(ctx, st, current, e); err != nil {
		return State{}, err
	}
	l.noteMu.Lock()
	delete(l.last, pairKey{f, c}) // the next record reads the stored clearing
	l.noteMu.Unlock()
	st.ServerMajor = &current
	return st, nil
}

// serverMajor reads the server's major for a re-approval.
func (l *Limiter) serverMajor(ctx context.Context) (int, error) {
	if l.b.ServerMajor == nil {
		return 0, fmt.Errorf("%w: no PostgreSQL version source for %s", ErrUnavailable,
			l.b.Database)
	}
	current, err := l.b.ServerMajor(ctx)
	if err != nil || current <= 0 {
		return 0, fmt.Errorf("%w: read the PostgreSQL major of %s: %v", ErrUnavailable,
			l.b.Database, err)
	}
	return current, nil
}

// majorView answers the rows of one page from one read of the major.
type majorView struct {
	l       *Limiter
	current int
}

// majorView reads the server major once for a page; current 0 when it is
// unknown (then no row shows a cap).
func (l *Limiter) majorView(ctx context.Context) majorView {
	v := majorView{l: l}
	if l.b.ServerMajor != nil {
		if m, err := l.b.ServerMajor(ctx); err == nil && m > 0 {
			v.current = m
		}
	}
	return v
}

// capOf is st's major cap as a page shows it (read only: nothing is
// stamped); nil when none holds.
func (v majorView) capOf(ctx context.Context, st State) *MajorCap {
	if v.current == 0 || !reValidates(st) || st.ServerMajor == nil ||
		*st.ServerMajor == v.current {
		return nil
	}
	n, err := v.l.svc.store.improvedSinceMajorCap(ctx, st.Family, st.Class, v.current)
	if err != nil {
		v.l.svc.cfg.logf("autonomy: count improved outcomes of %s/%s on %s: %v", st.Family,
			st.Class, v.l.b.Database, err)
	}
	need := v.l.svc.cfg.Thresholds.ClassMinSuccessesL2
	return &MajorCap{From: *st.ServerMajor, To: v.current, Improved: n, Needed: need,
		Text: MajorCapText(*st.ServerMajor, v.current, n, need)}
}
