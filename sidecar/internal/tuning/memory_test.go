package tuning

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// Case memory: the model is not asked about a case again while its recent
// answers were all wasted and the case has not changed materially (the
// W2-C streak rule, per case instead of per table).

func memSettings() caseMemorySettings {
	return caseMemorySettings{SkipAfter: 3, MaxAge: 7 * 24 * time.Hour, CallRatio: 2,
		MeanRatio: 2}
}

func memCase(calls int64, mean float64) Case {
	return Case{ID: "top_statement:1", Kind: CaseTopStatement,
		Statements: []CaseStatement{{QueryID: 1, Calls: calls, MeanMs: mean}}}
}

func TestCaseMemory_SkipsAfterNWastedAnswers(t *testing.T) {
	m := newCaseMemory(memSettings())
	c := memCase(1000, 10)
	for i := 0; i < 2; i++ {
		m.note(c, caseOutcome{Proposals: 1, Wasted: 1}, t0)
		if m.skip(c, t0) {
			t.Fatalf("skipped after %d wasted answers, want 3", i+1)
		}
	}
	m.note(c, caseOutcome{Proposals: 2, Wasted: 2}, t0)
	if !m.skip(c, t0) {
		t.Fatal("three wasted answers in a row: skip")
	}
}

func TestCaseMemory_EmptyAndMalformedAnswersCount(t *testing.T) {
	m := newCaseMemory(memSettings())
	c := memCase(1000, 10)
	m.note(c, caseOutcome{}, t0)                // "no change warranted"
	m.note(c, caseOutcome{Malformed: true}, t0) // unparseable answer
	m.note(c, caseOutcome{Proposals: 0}, t0)    // empty again
	if !m.skip(c, t0) {
		t.Fatal("an answer with nothing usable spent tokens: it counts as wasted")
	}
}

func TestCaseMemory_ProviderFailureIsNeutral(t *testing.T) {
	m := newCaseMemory(memSettings())
	c := memCase(1000, 10)
	m.note(c, caseOutcome{Proposals: 1, Wasted: 1}, t0)
	m.note(c, caseOutcome{Proposals: 1, Wasted: 1}, t0)
	m.note(c, caseOutcome{Failed: true}, t0)
	if m.skip(c, t0) {
		t.Fatal("a provider failure neither extends nor breaks the streak")
	}
	m.note(c, caseOutcome{Proposals: 1, Wasted: 1}, t0)
	if !m.skip(c, t0) {
		t.Fatal("the streak continued across the failure")
	}
}

func TestCaseMemory_UsefulAnswerResets(t *testing.T) {
	m := newCaseMemory(memSettings())
	c := memCase(1000, 10)
	m.note(c, caseOutcome{Proposals: 1, Wasted: 1}, t0)
	m.note(c, caseOutcome{Proposals: 1, Wasted: 1}, t0)
	m.note(c, caseOutcome{Proposals: 2, Wasted: 1}, t0) // one admitted
	m.note(c, caseOutcome{Proposals: 1, Wasted: 1}, t0)
	if m.skip(c, t0) {
		t.Fatal("an admitted proposal restarts the count")
	}
}

func TestCaseMemory_MaterialChangeLiftsTheSkip(t *testing.T) {
	for name, changed := range map[string]Case{
		"calls doubled": memCase(2000, 10),
		"calls halved":  memCase(500, 10),
		"mean doubled":  memCase(1000, 20),
		"statement changed": {ID: "top_statement:1",
			Statements: []CaseStatement{{QueryID: 2, Calls: 1000, MeanMs: 10}}},
	} {
		t.Run(name, func(t *testing.T) {
			m := newCaseMemory(memSettings())
			c := memCase(1000, 10)
			for i := 0; i < 3; i++ {
				m.note(c, caseOutcome{Proposals: 1, Wasted: 1}, t0)
			}
			if m.skip(changed, t0) {
				t.Fatal("a material change must ask the model again")
			}
		})
	}
	m := newCaseMemory(memSettings())
	c := memCase(1000, 10)
	for i := 0; i < 3; i++ {
		m.note(c, caseOutcome{Proposals: 1, Wasted: 1}, t0)
	}
	if !m.skip(memCase(1999, 19.99), t0) {
		t.Fatal("1.999x is not a material change")
	}
}

func TestCaseMemory_MaxAgeLiftsTheSkip(t *testing.T) {
	m := newCaseMemory(memSettings())
	c := memCase(1000, 10)
	for i := 0; i < 3; i++ {
		m.note(c, caseOutcome{Proposals: 1, Wasted: 1}, t0)
	}
	if !m.skip(c, t0.Add(7*24*time.Hour-time.Second)) {
		t.Fatal("just under the max age still skips")
	}
	if m.skip(c, t0.Add(7*24*time.Hour)) {
		t.Fatal("at the max age the model is asked again")
	}
}

func TestCaseMemory_CasesAreIndependentAndNilIsInert(t *testing.T) {
	m := newCaseMemory(memSettings())
	a, b := memCase(1000, 10), memCase(1000, 10)
	b.ID = "top_statement:2"
	for i := 0; i < 3; i++ {
		m.note(a, caseOutcome{Proposals: 1, Wasted: 1}, t0)
	}
	if !m.skip(a, t0) || m.skip(b, t0) {
		t.Fatal("streaks are per case")
	}
	var none *caseMemory
	none.note(a, caseOutcome{Proposals: 1, Wasted: 1}, t0)
	if none.skip(a, t0) {
		t.Fatal("a nil memory never skips")
	}
	off := newCaseMemory(caseMemorySettings{})
	for i := 0; i < 10; i++ {
		off.note(a, caseOutcome{Proposals: 1, Wasted: 1}, t0)
	}
	if off.skip(a, t0) {
		t.Fatal("SkipAfter 0 disables skipping")
	}
}

func TestCaseMemory_ConcurrentNotes(t *testing.T) {
	m := newCaseMemory(memSettings())
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := memCase(1000, 10)
			c.ID = fmt.Sprintf("top_statement:%d", i%5)
			m.note(c, caseOutcome{Proposals: 1, Wasted: 1}, t0)
			_ = m.skip(c, t0)
		}(i)
	}
	wg.Wait()
	for i := 0; i < 5; i++ {
		c := memCase(1000, 10)
		c.ID = fmt.Sprintf("top_statement:%d", i)
		if !m.skip(c, t0) {
			t.Fatalf("case %d had 10 wasted answers", i)
		}
	}
}
