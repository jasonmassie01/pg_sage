package earned

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// A pair's cap is recorded when what caps it changes (capped /
// cap_cleared), not on every authorization. Two sources cap a pair: the
// CHECK-40 downgrade signals of an authorization (at most L1), and, since
// earned-06, evidence that no longer supports its earned level (at most
// the supported level). The last recorded state is kept in memory and,
// before any write, compared with the pair's newest stored cap event, so
// a restart or a second limiter does not record the same cap again.

// capState is what caps a pair: the downgrade signals and the decay.
type capState struct {
	downs []Downgrade
	decay *decayNote
}

// key is the state's sorted reasons; "" when nothing caps the pair.
func (s capState) key() string {
	reasons := make([]string, 0, len(s.downs)+1)
	for _, d := range s.downs {
		reasons = append(reasons, d.Reason)
	}
	if s.decay != nil {
		reasons = append(reasons, s.decay.reason())
	}
	sort.Strings(reasons)
	return strings.Join(reasons, ",")
}

// capEvidence is what a cap event keeps of its state.
type capEvidence struct {
	Downgrades []Downgrade `json:"downgrades"`
	Granted    *Level      `json:"granted,omitempty"`
	Supported  *Level      `json:"supported,omitempty"`
	Failing    []string    `json:"failing_checks,omitempty"`
}

// unreadableCap stands for a stored cap whose evidence cannot be read: it
// differs from every real state, so the next transition is recorded.
const unreadableCap = "unreadable_cap_event"

// capEvent is the history entry for a change from prev to next. The
// CHECK-40 reason texts are unchanged.
func capEvent(f Family, c ActionClass, prev, next capState, at time.Time) Event {
	e := Event{Family: f, Class: c, Type: EventCapped, Actor: ActorPgSage, At: at}
	key := next.key()
	switch {
	case key == "" && prev.decay != nil && len(prev.downs) == 0:
		e.Type = EventCapCleared
		e.Reason = fmt.Sprintf("the evidence supports granted %s again", prev.decay.Granted)
	case key == "":
		e.Type, e.Reason = EventCapCleared, "downgrade signals cleared"
	case len(next.downs) > 0:
		e.Reason = "downgraded to at most L1: " + key
	default:
		e.Reason = fmt.Sprintf("the evidence supports %s of granted %s: %s",
			next.decay.Supported, next.decay.Granted, key)
	}
	ev := capEvidence{Downgrades: next.downs}
	if next.decay != nil {
		ev.Granted, ev.Supported = &next.decay.Granted, &next.decay.Supported
		ev.Failing = next.decay.Checks
	}
	raw, err := json.Marshal(ev)
	if err != nil { // plain fields: cannot fail
		raw = []byte(`{}`)
	}
	e.Evidence = raw
	return e
}

// capStateOf is the state a stored cap event recorded: nothing for none
// or a cap_cleared. Events before earned-06 carry downgrades only.
func capStateOf(e *Event) capState {
	if e == nil || e.Type != EventCapped {
		return capState{}
	}
	var ev capEvidence
	if err := json.Unmarshal(e.Evidence, &ev); err != nil {
		return capState{downs: []Downgrade{{Reason: unreadableCap, Detail: err.Error()}}}
	}
	out := capState{downs: ev.Downgrades}
	if ev.Granted != nil || len(ev.Failing) > 0 {
		d := &decayNote{Checks: ev.Failing}
		if ev.Granted != nil {
			d.Granted = *ev.Granted
		}
		if ev.Supported != nil {
			d.Supported = *ev.Supported
		}
		out.decay = d
	}
	return out
}

// noteTransition records the pair's cap when next (given the recorded
// state) differs from it, and returns the type recorded. A failed read or
// write is logged and not remembered, so the next call retries; the cap
// itself never waits on its record.
func (l *Limiter) noteTransition(ctx context.Context, f Family, c ActionClass,
	next func(prev capState) capState) (EventType, bool) {
	key := pairKey{f, c}
	l.noteMu.Lock()
	defer l.noteMu.Unlock()
	prev, remembered := l.last[key]
	if want := next(prev); remembered && want.key() == prev.key() {
		return "", false
	}
	stored, err := l.svc.store.latestEvent(ctx, f, c, EventCapped, EventCapCleared)
	if err != nil {
		l.svc.cfg.logf("autonomy: read the last cap of %s/%s on %s: %v", f, c,
			l.b.Database, err)
		return "", false
	}
	prev = capStateOf(stored)
	want := next(prev)
	if want.key() == prev.key() {
		l.last[key] = want
		return "", false
	}
	e := capEvent(f, c, prev, want, l.svc.now())
	if err := l.svc.store.appendEvent(ctx, l.svc.store.pool, e); err != nil {
		l.svc.cfg.logf("autonomy: record %s for %s/%s on %s: %v", e.Type, f, c,
			l.b.Database, err)
		return "", false
	}
	l.last[key] = want
	return e.Type, true
}

// pairEvidence reads a pair's supported level and evidence once.
type pairEvidence struct {
	svc   *Service
	f     Family
	c     ActionClass
	done  bool
	level Level
	ev    Evidence
	err   error
}

func (p *pairEvidence) get(ctx context.Context) (Level, Evidence, error) {
	if !p.done {
		p.level, p.ev, p.err = p.svc.supportedEvidence(ctx, p.f, p.c)
		p.done = true
	}
	return p.level, p.ev, p.err
}

// noteLimit records an authorization's cap: its downgrade signals and the
// pair's decay. When the evidence cannot be read the recorded decay is
// kept (the gate already failed closed if it needed the level).
func (l *Limiter) noteLimit(ctx context.Context, st State, downs []Downgrade,
	p *pairEvidence) {
	var decay *decayNote
	known := true
	if st.Provenance != ProvenanceCarriedOver && !IsSelfInitiated(st.Family) {
		supported, ev, err := p.get(ctx)
		if err != nil {
			known = false
		} else {
			decay = decayOf(l.svc.cfg.Thresholds, st, supported, ev)
		}
	}
	l.noteTransition(ctx, st.Family, st.Class, func(prev capState) capState {
		if !known {
			return capState{downs: downs, decay: prev.decay}
		}
		return capState{downs: downs, decay: decay}
	})
}
