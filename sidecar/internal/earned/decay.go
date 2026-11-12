package earned

import (
	"context"
	"slices"
	"sort"
	"strings"
	"time"
)

// earned-06: an earned incident level holds only while the evidence that
// earned it does. The bench report ages out BenchMaxAge after it was
// generated, and shadow reviews leave the ShadowDuration window one by
// one. The ledger says when the evidence a pair holds now stops holding
// (EvidenceExpiry), records the decay as a cap without waiting for an
// action (Limiter.Sweep) and warns ExpiryWarningLead ahead, once. The
// thresholds, SupportedLevel and the carried-over and self-initiated
// exemptions are unchanged.

// ExpiryWarningLead is how far ahead of its evidence expiring an earned
// level is warned about.
const ExpiryWarningLead = 7 * 24 * time.Hour

// maxReviewMarks bounds the shadow reviews one family's expiry reads
// (oldest first); a window larger than that has no near expiry.
const maxReviewMarks = 10000

// decayReasonPrefix names decayed evidence among a cap's reasons.
const decayReasonPrefix = "evidence_decayed:"

// reviewMark is one shadow review in the window: when, and whether it was
// accepted.
type reviewMark struct {
	at       time.Time
	accepted bool
}

// decayNote is a pair whose granted level its evidence no longer
// supports, and the checks of the granted level that fail.
type decayNote struct {
	Granted   Level
	Supported Level
	Checks    []string
}

// decayOf is st's decay under ev (supported is SupportedLevel of ev); nil
// below L2, while the evidence supports the level, and for a carried-over
// or self-initiated level (those do not decay with evidence).
func decayOf(th Thresholds, st State, supported Level, ev Evidence) *decayNote {
	if st.Level < L2 || supported >= st.Level || st.Provenance == ProvenanceCarriedOver ||
		IsSelfInitiated(st.Family) {
		return nil
	}
	var checks []string
	for _, c := range Assess(th, st.Level, ev).Checks {
		if !c.Met {
			checks = append(checks, c.Name)
		}
	}
	sort.Strings(checks)
	return &decayNote{Granted: st.Level, Supported: supported, Checks: checks}
}

// expiryOf is the earliest time a currently met decaying check of ev
// stops being met (the last instant it still holds), and those checks;
// zero when none of them is met. marks are the family's reviews in the
// shadow window (any order).
func expiryOf(th Thresholds, ev Evidence, marks []reviewMark) (time.Time, []string) {
	var at time.Time
	var checks []string
	add := func(t time.Time, names []string) {
		switch {
		case t.IsZero():
		case at.IsZero() || t.Before(at):
			at, checks = t, slices.Clone(names)
		case t.Equal(at):
			checks = append(checks, names...)
		}
	}
	if ev.Bench != nil && benchFresh(th, ev).Met {
		add(ev.Bench.GeneratedAt.Add(th.BenchMaxAge), []string{"bench_fresh"})
	}
	add(shadowExpiry(th, ev, marks))
	sort.Strings(checks)
	return at, checks
}

// shadowExpiry removes the window's reviews oldest first (reviews of one
// instant together) and returns when the first removal breaks a met
// shadow_volume or shadow_acceptance check. A read that ends before any
// check breaks was truncated: no expiry rather than a wrong one.
func shadowExpiry(th Thresholds, ev Evidence, marks []reviewMark) (time.Time, []string) {
	n, k := ev.Shadow.Reviewed, ev.Shadow.Accepted
	heldVolume, heldAcceptance := shadowMet(th, n, k)
	if !heldVolume && !heldAcceptance {
		return time.Time{}, nil
	}
	since := ev.At.Add(-th.ShadowDuration)
	window := make([]reviewMark, 0, len(marks))
	for _, m := range marks {
		if !m.at.Before(since) {
			window = append(window, m)
		}
	}
	sort.SliceStable(window, func(i, j int) bool { return window[i].at.Before(window[j].at) })
	for i := 0; i < len(window); {
		t := window[i].at
		for ; i < len(window) && window[i].at.Equal(t); i++ {
			n--
			if window[i].accepted {
				k--
			}
		}
		volume, acceptance := shadowMet(th, n, k)
		var broken []string
		if heldAcceptance && !acceptance {
			broken = append(broken, "shadow_acceptance")
		}
		if heldVolume && !volume {
			broken = append(broken, "shadow_volume")
		}
		if len(broken) > 0 {
			return t.Add(th.ShadowDuration), broken
		}
	}
	return time.Time{}, nil
}

// needsMarks reports whether ev's shadow window can expire (one of its
// window checks is met), so its reviews are worth reading.
func needsMarks(th Thresholds, ev Evidence) bool {
	volume, acceptance := shadowMet(th, ev.Shadow.Reviewed, ev.Shadow.Accepted)
	return volume || acceptance
}

// EvidenceExpiry is when the pair's currently met decaying evidence (the
// bench report's age, the shadow window) first stops holding, and which
// checks stop; zero when nothing met decays, and for a self-initiated
// class (its record does not age out). A read failure is an error.
func (s *Service) EvidenceExpiry(ctx context.Context, f Family, c ActionClass) (time.Time,
	[]string, error) {
	if IsSelfInitiated(f) {
		return time.Time{}, nil, nil
	}
	ev, err := s.Evidence(ctx, f, c)
	if err != nil {
		return time.Time{}, nil, err
	}
	return s.expiryAt(ctx, ev)
}

// expiryAt is expiryOf for ev, reading the family's shadow reviews when
// its window can expire.
func (s *Service) expiryAt(ctx context.Context, ev Evidence) (time.Time, []string, error) {
	th := s.cfg.Thresholds
	var marks []reviewMark
	if needsMarks(th, ev) {
		set := map[Family][]reviewMark{}
		err := s.store.readReviewMarks(ctx, s.store, []string{string(ev.Family)},
			ev.At.Add(-th.ShadowDuration), set)
		if err != nil {
			return time.Time{}, nil, err
		}
		marks = set[ev.Family]
	}
	at, checks := expiryOf(th, ev, marks)
	return at, checks, nil
}

// holding is when st's earned level, which ev supports now, stops being
// supported, for a page's row; nil for a level that does not decay or
// that the evidence no longer supports.
func (s *Service) holding(st State, supported Level, ev Evidence,
	marks []reviewMark) (*time.Time, []string) {
	if st.Level < L2 || supported < st.Level || st.Provenance == ProvenanceCarriedOver ||
		IsSelfInitiated(st.Family) {
		return nil, nil
	}
	at, checks := expiryOf(s.cfg.Thresholds, ev, marks)
	if at.IsZero() {
		return nil, nil
	}
	return &at, checks
}

// decayReason is a decay's element among a cap's reasons.
func (d *decayNote) reason() string {
	return decayReasonPrefix + strings.Join(d.Checks, ",")
}
