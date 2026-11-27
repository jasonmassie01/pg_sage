package approvalcard

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/store"
)

// The trust of a card's action class on its database (roadmap 1.2): the
// level the ledger grants, its evidence counts and the path to the next
// level, from the same view as the Trust page. It informs the decision;
// it is not part of the content a decision is bound to (ContentHash).

// TrustSource reads a database's annotated Trust view; false when the
// database has no ledger. *earned.Registry is one.
type TrustSource interface {
	TrustView(ctx context.Context, database string) (earned.TrustView, bool, error)
}

// Trust is the card's class trust. Line is the one line chat shows.
type Trust struct {
	Family      string             `json:"family,omitempty"`
	Class       string             `json:"class,omitempty"`
	Level       string             `json:"level,omitempty"`
	Effective   string             `json:"effective,omitempty"`
	Cap         string             `json:"cap,omitempty"`
	Provenance  string             `json:"provenance,omitempty"`
	Evidence    earned.TrustCounts `json:"evidence"`
	NextLevel   string             `json:"next_level,omitempty"`
	Path        []string           `json:"path,omitempty"`
	Unavailable string             `json:"unavailable,omitempty"`
	// Bench is the deciding bench report's provenance label, BenchOrigin
	// its origin and BenchID its id (none without bench evidence).
	Bench       string `json:"bench,omitempty"`
	BenchOrigin string `json:"bench_origin,omitempty"`
	BenchID     string `json:"bench_id,omitempty"`
	// AllowAlways offers "Approve, and stop asking for <class> on
	// <database>" (ux-07a), labelled AllowAlwaysLabel; AllowAlwaysWhy is
	// why it is offered or not. ApprovalHistory is the class's person
	// approvals in the window.
	AllowAlways      bool                    `json:"allow_always"`
	AllowAlwaysLabel string                  `json:"allow_always_label,omitempty"`
	AllowAlwaysWhy   string                  `json:"allow_always_why,omitempty"`
	ApprovalHistory  *earned.ApprovalHistory `json:"approval_history,omitempty"`
	Line             string                  `json:"line"`
}

// trustRead is one read of a database's Trust view.
type trustRead struct {
	view earned.TrustView
	ok   bool
	err  error
}

// readTrust reads the loader's database view once (nothing without a
// source).
func (l Loader) readTrust(ctx context.Context) trustRead {
	if l.Trust == nil {
		return trustRead{}
	}
	v, ok, err := l.Trust.TrustView(ctx, l.Database)
	return trustRead{view: v, ok: ok, err: err}
}

// apply puts the read's trust of a's class on in.
func (t trustRead) apply(in *Inputs, a store.QueuedAction) {
	switch {
	case t.err != nil:
		in.TrustErr = t.err
	case t.ok:
		in.Trust = trustRowFor(t.view, a)
	}
}

// trustRowFor is the view's row of a's ledger pair, nil when the ledger
// does not judge its class.
func trustRowFor(v earned.TrustView, a store.QueuedAction) *earned.TrustRow {
	f, c := earned.PairForQueued(actionTypeOf(a), a.ProposedSQL, a.IdentityKey)
	if f == "" {
		return nil
	}
	for i := range v.Rows {
		if v.Rows[i].Family == f && v.Rows[i].Class == c {
			row := v.Rows[i]
			return &row
		}
	}
	return nil
}

// trustOf is the card's trust from its inputs (nil without a ledger).
func trustOf(in Inputs) *Trust {
	if in.TrustErr != nil {
		why := in.TrustErr.Error()
		return &Trust{Unavailable: why, Line: "Trust: unavailable (" + why + ")"}
	}
	row := in.Trust
	if row == nil {
		return nil
	}
	t := &Trust{Family: string(row.Family), Class: string(row.Class),
		Level: row.Level.String(), Cap: row.Cap.String(), Provenance: row.Provenance,
		Evidence: row.Evidence}
	if row.Effective != nil {
		t.Effective = row.Effective.String()
	}
	if b := row.Bench; b != nil {
		t.Bench, t.BenchOrigin, t.BenchID = b.Provenance, b.Origin, b.ID
	}
	if row.Next != nil {
		t.NextLevel = row.Next.Target.String()
		t.Path = pathOf(*row.Next)
	}
	t.ApprovalHistory = row.ApprovalHistory
	allowAlwaysOf(t, *row, in)
	t.Line = trustLine(t)
	return t
}

// allowAlwaysOf decides the card's allow-always button by the ledger's
// rule against the database's gate inputs; without them (no executor)
// it is never offered.
func allowAlwaysOf(t *Trust, row earned.TrustRow, in Inputs) {
	if in.Ceiling == nil {
		t.AllowAlwaysWhy = "the gate's inputs are not known (no executor)"
		return
	}
	t.AllowAlways, t.AllowAlwaysWhy = earned.AllowAlways(row, *in.Ceiling)
	if t.AllowAlways {
		t.AllowAlwaysLabel = earned.AllowAlwaysLabel(row.Class, in.Database)
	}
}

// pathOf lists what the next level still needs.
func pathOf(a earned.Assessment) []string {
	var out []string
	for _, c := range a.Checks {
		if c.Met {
			continue
		}
		step := strings.TrimRight(strings.TrimSpace(c.How), ".")
		if step == "" {
			step = fmt.Sprintf("%s: %s of %s", c.Name, orUnknown(c.Observed), c.Required)
		}
		if c.ETA != nil {
			step += " (ETA " + c.ETA.UTC().Format("2006-01-02 15:04 UTC") + ")"
		}
		out = append(out, step)
	}
	return out
}

// trustLine renders the trust as one chat line.
func trustLine(t *Trust) string {
	level := t.Level
	var notes []string
	if t.Effective != "" {
		notes = append(notes, "effective "+t.Effective)
	}
	notes = append(notes, "cap "+t.Cap)
	if t.Provenance != "" && t.Provenance != earned.ProvenanceLedger {
		notes = append(notes, t.Provenance)
	}
	e := t.Evidence
	line := fmt.Sprintf("Trust: %s/%s at %s (%s); evidence %d improved, %d neutral, "+
		"%d regressed, %d rolled back, %d rejected", t.Family, t.Class, level,
		strings.Join(notes, ", "), e.Improved, e.Neutral, e.Regressed, e.RolledBack,
		e.Rejected)
	if t.Bench != "" {
		line += strings.TrimRight("; bench: "+t.Bench+" "+t.BenchID, " ")
	}
	switch {
	case t.NextLevel == "":
		return line + "; at its highest level"
	case len(t.Path) == 0:
		return line + "; next " + t.NextLevel + ": ready to propose"
	}
	return line + "; next " + t.NextLevel + ": " + strings.Join(t.Path, "; ")
}
