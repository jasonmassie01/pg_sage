package briefing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// review-05a: the digest states the gate's changes. No concurrent access
// test: one digest reads the gate once.

func TestBuildDigest_ModelGateSectionOnlyWithChanges(t *testing.T) {
	plain := BuildDigest(DigestInput{Database: "shop"})
	if len(plain.Sections) != 5 {
		t.Fatalf("no gate notes: %d sections, want the five fixed ones", len(plain.Sections))
	}
	notes := []string{"G-M1 granted for model m1 by admin: its steps now run at their " +
		"class's trust level", "G-M2: the review's model pass stopped calling m1: none"}
	d := BuildDigest(DigestInput{Database: "shop", GateNotes: notes})
	last := d.Sections[len(d.Sections)-1]
	if len(d.Sections) != 6 || last.Key != SectionModelGate || last.Summary != "2 changes" ||
		len(last.Items) != 2 || last.Items[0].Label != notes[0] {
		t.Fatalf("gate section = %+v", last)
	}
	if d.Hash == plain.Hash || !strings.Contains(d.Text(), "Model gate: 2 changes") ||
		!strings.Contains(d.Text(), "- G-M2: the review's model pass stopped calling m1") {
		t.Fatalf("digest text %q does not state the gate", d.Text())
	}
	again := BuildDigest(DigestInput{Database: "shop", GateNotes: []string{}})
	if again.Hash != plain.Hash {
		t.Fatal("an empty gate read changed the digest hash")
	}
}

func TestBuildDigest_GateReadErrorIsAGap(t *testing.T) {
	d := BuildDigest(DigestInput{Database: "shop", GateNotesErr: errors.New("refused")})
	found := false
	for _, g := range d.Gaps {
		found = found || g == "the review model gate could not be read"
	}
	if !found || len(d.Sections) != 5 {
		t.Fatalf("gaps %v, %d sections, want the gate named as a gap", d.Gaps,
			len(d.Sections))
	}
}

func TestGateSection_CapsItsLines(t *testing.T) {
	notes := make([]string, maxSectionItems+3)
	for i := range notes {
		notes[i] = "G-M1 revoked for model m1: reverts"
	}
	s, ok := gateSection(notes)
	if !ok || len(s.Items) != maxSectionItems || s.Summary != "13 changes" {
		t.Fatalf("section = %+v", s)
	}
	if one, _ := gateSection(notes[:1]); one.Summary != "1 change" {
		t.Fatalf("one note: %q", one.Summary)
	}
}

func TestReadGate_WiredUnwiredAndFailing(t *testing.T) {
	win := Window{Start: time.Unix(0, 0), End: time.Unix(3600, 0)}
	w := &Worker{database: "shop", logFn: func(string, string, ...any) {}}
	if got, err := w.readGate(context.Background(), win); err != nil || got != nil {
		t.Fatalf("unwired: %v %v", got, err)
	}
	var since, until time.Time
	w.WithGateNotes(func(_ context.Context, s, u time.Time) ([]string, error) {
		since, until = s, u
		return []string{"x"}, nil
	})
	if got, err := w.readGate(context.Background(), win); err != nil || len(got) != 1 ||
		!since.Equal(win.Start) || !until.Equal(win.End) {
		t.Fatalf("wired: %v %v (%v..%v)", got, err, since, until)
	}
	boom := errors.New("relation sage.review_gate does not exist")
	w.WithGateNotes(func(context.Context, time.Time, time.Time) ([]string, error) {
		return nil, boom
	})
	if _, err := w.readGate(context.Background(), win); !errors.Is(err, boom) {
		t.Fatalf("failing: %v, want the error", err)
	}
}
