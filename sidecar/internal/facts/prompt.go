package facts

import (
	"context"
	"fmt"
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

// PromptLines renders the confirmed, unexpired facts relevant to objects
// (all of them when objects is empty) as bounded prompt lines in fact
// order. Proposed, rejected and expired facts are never included.
func PromptLines(facts []Fact, now time.Time, objects ...string) []string {
	refs := make([]ObjectRef, 0, len(objects))
	for _, o := range objects {
		if ref, err := ParseObjectRef(o); err == nil {
			refs = append(refs, ref)
		}
	}
	var out []string
	used := 0
	for _, f := range facts {
		if !f.binds(now) || (len(objects) > 0 && !relevant(f, refs)) {
			continue
		}
		line := clip(promptLine(f), maxPromptLine)
		if len(out) == maxPromptLines || used+len(line)+1 > maxPromptChars {
			break
		}
		out = append(out, line)
		used += len(line) + 1
	}
	return out
}

func promptLine(f Fact) string {
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
// any object in its schema (the object's indexes are not listed).
func relevant(f Fact, refs []ObjectRef) bool {
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
// model prompt (optimizer, advisor, investigator). A read error yields no
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
