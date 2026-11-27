package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
)

// scriptModel is a scripted triage and fleet model: fn answers each call
// and every call's purpose and prompt are kept.
type scriptModel struct {
	mu       sync.Mutex
	fn       func(system, user string) (llm.Response, error)
	purposes []string
	prompts  []string
}

func (m *scriptModel) ChatResult(ctx context.Context, system, user string,
	_ int) (llm.Response, error) {
	m.mu.Lock()
	m.purposes = append(m.purposes, llm.PurposeOf(ctx))
	m.prompts = append(m.prompts, user)
	m.mu.Unlock()
	return m.fn(system, user)
}

func (m *scriptModel) Model() string { return "haiku-class" }

func (m *scriptModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.purposes)
}

// reply is a model answer charged at a quarter token per prompt byte.
func reply(user, content string) llm.Response {
	return llm.Response{Content: content, PromptTokens: len(user) / 4,
		CompletionTokens: len(content) / 4, Tokens: (len(user) + len(content)) / 4}
}

// promptLines are the rollup lines of a triage prompt.
func promptLines(t testing.TB, user string) []triageLine {
	var out []triageLine
	for _, line := range strings.Split(user, "\n") {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var l triageLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Errorf("prompt line %q: %v", line, err)
			continue
		}
		out = append(out, l)
	}
	return out
}

// namesDrops is a triage model that names every member whose grade fell
// by at least 20 points, citing its own rollup id.
func namesDrops(t testing.TB) *scriptModel {
	return &scriptModel{fn: func(system, user string) (llm.Response, error) {
		if !strings.Contains(system, "outliers") {
			return reply(user, "The fleet is mostly healthy."), nil
		}
		type item struct {
			Member string   `json:"member"`
			Reason string   `json:"reason"`
			Cites  []string `json:"cites"`
		}
		var items []item
		for _, l := range promptLines(t, user) {
			if l.Drop >= 20 {
				items = append(items, item{l.ID, "grade fell", []string{l.ID}})
			}
		}
		b, _ := json.Marshal(map[string]any{"outliers": items})
		return reply(user, string(b)), nil
	}}
}

// constModel answers every call with content and err.
func constModel(content string, err error) *scriptModel {
	return &scriptModel{fn: func(_, user string) (llm.Response, error) {
		return reply(user, content), err
	}}
}

// synth is a fleet of n members db0001..; the members in drop lost 30
// grade points since their last review, the rest are steady at 85.
func synth(n int, drop ...int) map[string]Rollup {
	out := make(map[string]Rollup, n)
	dropped := map[int]bool{}
	for _, i := range drop {
		dropped[i] = true
	}
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("db%04d", i)
		prev := 85.0
		r := Rollup{Database: name, Graded: true, Grade: "B", Score: 85, PrevScore: &prev,
			CoveragePct: 90, Candidates: map[string]int{"indexes": 1}, TopImpact: 1,
			IdleKnown: true, SizeBytes: int64(i) << 20}
		if dropped[i] {
			prev = 90
			r.PrevScore, r.Score, r.Grade = &prev, 60, "D"
		}
		out[name] = r
	}
	return out
}

func names(m map[string]Rollup) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// mapSource serves fixed rollups; errs fail a member's read.
type mapSource struct {
	rollups map[string]Rollup
	errs    map[string]error
	reads   atomic.Int64
}

func (s *mapSource) Rollup(_ context.Context, db string) (Rollup, error) {
	s.reads.Add(1)
	if err := s.errs[db]; err != nil {
		return Rollup{}, err
	}
	r, ok := s.rollups[db]
	if !ok {
		return Rollup{}, fmt.Errorf("no rollup for %s", db)
	}
	return r, nil
}

// fakeDeep records scheduled deep reviews; fail refuses some databases
// and done marks a reference reviewed by a run.
type fakeDeep struct {
	mu        sync.Mutex
	scheduled []Deep
	fail      map[string]error
	done      map[int64]int64
	next      int64
}

func (f *fakeDeep) ScheduleDeep(_ context.Context, d Deep) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail[d.Database]; err != nil {
		return 0, err
	}
	f.next++
	f.scheduled = append(f.scheduled, d)
	return f.next, nil
}

func (f *fakeDeep) DeepStatus(_ context.Context, _ string, ref int64) (bool, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	run, ok := f.done[ref]
	return ok, run, nil
}

// clock is a settable time for the runner.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }
