package facts

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Prompt bounds: confirmed facts are context, never the bulk of a prompt.
const (
	maxPromptLines = 12
	maxPromptLine  = 300
	maxPromptChars = 1500
)

// guidance says what a confirmed fact means for a proposal.
var guidance = map[Type]string{
	TypeAppMigrations: "Never propose creating, dropping or altering it with DDL; " +
		"propose the change as an application migration instead.",
	TypeTestFixture: "These are test fixtures, not workload: ignore them.",
	TypeSlotConsumer: "Never drop or advance it, and never bound WAL in a way that " +
		"could invalidate it; alert instead.",
	TypeAppendOnly: "Its rows are never updated or deleted: keep its data and its " +
		"indexes (they serve rare reads).",
}

// promptPriority orders the facts of a prompt before its budget cuts: the
// owner brief (about the whole database), then the facts behind WAL and
// vacuum advice, so many index ownership facts never crowd them out. Ties
// go by fact id.
var promptPriority = map[Type]int{TypeOwnerBrief: 0, TypeSlotConsumer: 1,
	TypeTableWindow: 2, TypeAppendOnly: 3, TypeAppMigrations: 4, TypeTestFixture: 5}

func promptRank(t Type) int {
	if p, ok := promptPriority[t]; ok {
		return p
	}
	return len(promptPriority)
}

// PromptLines renders the confirmed, unexpired facts relevant to objects
// (all of them when objects is empty) as bounded prompt lines, by type
// priority and then fact order. Proposed, rejected and expired facts are
// never included. When the bounds cut facts, a last line says how many,
// and the lines with it stay within maxPromptChars.
func PromptLines(facts []Fact, now time.Time, objects ...string) []string {
	picked := promptFacts(facts, now, objects)
	var out []string
	used := 0
	for i, f := range picked {
		line := clip(promptLine(f), maxPromptLine)
		if len(out) == maxPromptLines || used+len(line)+1 > maxPromptChars {
			return withOmittedNote(out, used, len(picked)-i)
		}
		out = append(out, line)
		used += len(line) + 1
	}
	return out
}

// promptFacts are the binding facts relevant to objects, in prompt order;
// the caller's slice is left as it is.
func promptFacts(facts []Fact, now time.Time, objects []string) []Fact {
	refs := make([]ObjectRef, 0, len(objects))
	for _, o := range objects {
		if ref, err := ParseObjectRef(o); err == nil {
			refs = append(refs, ref)
		}
	}
	var picked []Fact
	for _, f := range facts {
		if f.binds(now) && (len(objects) == 0 || relevant(f, refs)) {
			picked = append(picked, f)
		}
	}
	sort.SliceStable(picked, func(i, j int) bool {
		ri, rj := promptRank(picked[i].Type), promptRank(picked[j].Type)
		return ri < rj || (ri == rj && picked[i].ID < picked[j].ID)
	})
	return picked
}

// withOmittedNote ends out (used characters) with a note naming the
// omitted facts, dropping fact lines until the note fits the budget.
func withOmittedNote(out []string, used, omitted int) []string {
	for {
		note := omittedNote(omitted)
		if used+len(note)+1 <= maxPromptChars || len(out) == 0 {
			return append(out, note)
		}
		used -= len(out[len(out)-1]) + 1
		out = out[:len(out)-1]
		omitted++
	}
}

func omittedNote(n int) string {
	if n == 1 {
		return "- 1 more confirmed fact omitted."
	}
	return fmt.Sprintf("- %d more confirmed facts omitted.", n)
}

func promptLine(f Fact) string {
	if f.Type == TypeOwnerBrief {
		return briefPromptLine(f)
	}
	how := guidance[f.Type]
	if f.Type == TypeTableWindow {
		how = "Act on it only inside that window."
		if f.Value["kind"] == "batch" {
			how = "Do not act on it during that window."
		}
	}
	return cleanText(fmt.Sprintf("- fact #%d (operator-confirmed): %s. %s", f.ID,
		f.Describe(), how))
}

// relevant reports a fact about one of refs; an index fact is relevant to
// any object in its schema (the object's indexes are not listed), and a
// database fact (the owner brief) to every object.
func relevant(f Fact, refs []ObjectRef) bool {
	if f.Kind == KindDatabase {
		return true
	}
	p, err := ParsePattern(f.Kind, f.Subject)
	if err != nil {
		return false
	}
	for _, ref := range refs {
		if p.Matches(ref) || (p.Kind == KindIndex && ref.Kind != KindSlot &&
			ref.Schema != "" && globMatch(p.Schema, ref.Schema)) {
			return true
		}
	}
	return false
}

// PromptLines renders the database's confirmed facts about objects for a
// model prompt (advisor, investigator). A read error yields no
// lines: the prompt goes without them and the gate still binds.
func (s *Store) PromptLines(ctx context.Context, objects ...string) []string {
	facts, err := s.Confirmed(ctx)
	if err != nil {
		s.logFn("WARN", "facts: prompt context unavailable: %v", err)
		return nil
	}
	return PromptLines(facts, s.now(), objects...)
}

// Matching returns the facts (any status) whose subject covers object, a
// target such as "public.orders" or "slot:cdc", in the given order.
func Matching(facts []Fact, object string) []Fact {
	ref, err := ParseObjectRef(object)
	if err != nil {
		return nil
	}
	var out []Fact
	for _, f := range facts {
		if p, err := ParsePattern(f.Kind, f.Subject); err == nil && p.Matches(ref) {
			out = append(out, f)
		}
	}
	return out
}
