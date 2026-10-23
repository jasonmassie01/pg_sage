package changereview

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

// analyzerFunc adapts a function to Analyzer.
type analyzerFunc func(ctx context.Context, in Input) (Analysis, error)

func (f analyzerFunc) Analyze(ctx context.Context, in Input) (Analysis, error) {
	return f(ctx, in)
}

// reasonerFunc adapts a function to Reasoner.
type reasonerFunc func(ctx context.Context, in Input, a Analysis) (Reasoning, error)

func (f reasonerFunc) Reason(ctx context.Context, in Input, a Analysis) (Reasoning, error) {
	return f(ctx, in, a)
}

// mustEvidence is NewEvidence that fails the test on error.
func mustEvidence(t testing.TB, kind, ref, label string, data any) Evidence {
	t.Helper()
	e, err := NewEvidence(kind, ref, label, data)
	if err != nil {
		t.Fatalf("NewEvidence(%s:%s): %v", kind, ref, err)
	}
	return e
}

// staticAnalysis answers every review with one statement per SQL input
// and the given findings, citing a statement evidence.
func staticAnalysis(t testing.TB, findings ...Finding) analyzerFunc {
	return func(_ context.Context, in Input) (Analysis, error) {
		a := Analysis{ParseMode: ParseAST}
		for i, f := range in.SQLFiles {
			ev := mustEvidence(t, "stmt", strconv.Itoa(i+1), "statement "+strconv.Itoa(i+1),
				map[string]any{"sql": f.Content})
			a.Evidence = append(a.Evidence, ev)
			a.Statements = append(a.Statements, Statement{Index: i + 1,
				Origin: OriginSQLInput, File: f.Path, Line: 1, SQL: f.Content,
				Kind: "add_column", Relations: []string{"public.orders"},
				EvidenceID: ev.ID})
		}
		a.Findings = append(a.Findings, findings...)
		a.Expectations = Expectations{Relations: []string{"public.orders"},
			PostConditions: []PostCondition{{StatementIndex: 1, Kind: PostColumnPresent,
				Relation: "public.orders", Column: "note"}}}
		return a, nil
	}
}

// gate blocks analyses until released, counting how many are inside.
type gate struct {
	release chan struct{}
	inside  atomic.Int32
	peak    atomic.Int32
	entered chan struct{}
}

func newGate() *gate {
	return &gate{release: make(chan struct{}), entered: make(chan struct{}, 64)}
}

func (g *gate) analyzer(t testing.TB) analyzerFunc {
	inner := staticAnalysis(t)
	return func(ctx context.Context, in Input) (Analysis, error) {
		n := g.inside.Add(1)
		for {
			p := g.peak.Load()
			if n <= p || g.peak.CompareAndSwap(p, n) {
				break
			}
		}
		g.entered <- struct{}{}
		defer g.inside.Add(-1)
		select {
		case <-g.release:
		case <-ctx.Done():
			return Analysis{}, ctx.Err()
		}
		return inner(ctx, in)
	}
}

// recordingNotifier records ReviewNeedsDBA calls.
type recordingNotifier struct {
	mu      sync.Mutex
	reviews []Review
}

func (n *recordingNotifier) ReviewNeedsDBA(_ context.Context, r Review) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.reviews = append(n.reviews, r)
}

func (n *recordingNotifier) calls() []Review {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]Review(nil), n.reviews...)
}

var errStageBroken = errors.New("stage broken")

// ruleFinding is a deterministic finding citing ev.
func ruleFinding(rule, severity string, ev ...string) Finding {
	if len(ev) == 0 {
		ev = []string{"stmt:1"}
	}
	return Finding{RuleID: rule, Kind: "rule_" + rule, Severity: severity,
		Origin: OriginRule, Title: rule + " " + severity, Text: "deterministic " + rule,
		EvidenceIDs: ev}
}

// modelFinding is a model finding citing ev.
func modelFinding(severity string, ev ...string) Finding {
	if len(ev) == 0 {
		ev = []string{"stmt:1"}
	}
	return Finding{Kind: "n_plus_one", Severity: severity, Origin: OriginModel,
		Title: "model " + severity, Text: "the model says so", EvidenceIDs: ev}
}
