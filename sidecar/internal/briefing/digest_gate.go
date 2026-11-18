package briefing

import (
	"context"
	"time"
)

// The digest's model gate lines (review-05a): a G-M1 grant or revocation
// and G-M2 dropping or restoring the model pass, in the digest's window.
// The section is listed only when there is a change, so a digest with
// none is unchanged (and its hash with it).

// GateNotes lists the gate's changes in [since, until), one line each.
type GateNotes func(ctx context.Context, since, until time.Time) ([]string, error)

// WithGateNotes makes the digest list f's lines.
func (w *Worker) WithGateNotes(f GateNotes) *Worker {
	w.gate = f
	return w
}

// readGate is the window's gate lines; none when nothing is wired.
func (w *Worker) readGate(ctx context.Context, win Window) ([]string, error) {
	if w.gate == nil {
		return nil, nil
	}
	rctx, cancel := context.WithTimeout(ctx, digestSourceTimeout)
	defer cancel()
	notes, err := w.gate(rctx, win.Start, win.End)
	if err != nil {
		w.logFn("WARN", "briefing: [%s] digest: read the review model gate: %v", w.database,
			err)
		return nil, err
	}
	return notes, nil
}

// gateSection is the Model gate section of notes, or false without any.
func gateSection(notes []string) (Section, bool) {
	if len(notes) == 0 {
		return Section{}, false
	}
	s := Section{Key: SectionModelGate, Title: "Model gate",
		Summary: plural(len(notes), "change", "changes"), Items: []SectionItem{}}
	for _, n := range notes {
		if len(s.Items) == maxSectionItems {
			break
		}
		s.Items = append(s.Items, SectionItem{Label: n})
	}
	return s, true
}
