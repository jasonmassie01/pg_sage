package advisor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/facts"
)

// facts-11 CHECK-22: with thirty index-ownership facts confirmed, the
// advisor prompt still carries the slot fact WAL and vacuum advice need,
// and says how many facts the budget cut.

type renderedFacts struct {
	facts []facts.Fact
	now   time.Time
}

func (r renderedFacts) PromptLines(_ context.Context, objects ...string) []string {
	return facts.PromptLines(r.facts, r.now, objects...)
}

func manyIndexFactsAndASlot(now time.Time) []facts.Fact {
	at := now.Add(-time.Hour)
	var out []facts.Fact
	for i := 1; i <= 30; i++ {
		out = append(out, facts.Fact{ID: int64(i), Type: facts.TypeAppMigrations,
			Kind: facts.KindIndex, Subject: fmt.Sprintf("public.idx_orders_%02d", i),
			Status: facts.StatusConfirmed, DecidedBy: "op", DecidedAt: &at})
	}
	return append(out, facts.Fact{ID: 31, Type: facts.TypeSlotConsumer,
		Kind: facts.KindSlot, Subject: "cdc_orders", Status: facts.StatusConfirmed,
		Value: map[string]string{"consumer": "debezium"}, DecidedBy: "op", DecidedAt: &at})
}

func TestAdvisorPromptKeepsSlotFactAmongManyIndexFacts(t *testing.T) {
	mgr, last := capturingAdvisorLLM(t)
	now := time.Now()
	a := &Advisor{logFn: noopLog}
	a.WithFacts(renderedFacts{facts: manyIndexFactsAndASlot(now), now: now})
	ctx := a.factsContext(context.Background())
	block := promptFactsFrom(ctx)
	if !strings.HasPrefix(block, "- fact #31 ") {
		t.Fatalf("the slot fact must lead the facts block:\n%s", block)
	}
	if !strings.HasSuffix(block, "more confirmed facts omitted.") {
		t.Fatalf("the block must say facts were cut:\n%s", block)
	}
	if len(block) > maxFactsChars {
		t.Fatalf("block of %d chars", len(block))
	}
	if _, err := chatAdvisor(ctx, mgr, "wal", "system", "", "wal stats"); err != nil {
		t.Fatal(err)
	}
	body := last()
	for _, want := range []string{"replication slot cdc_orders belongs to debezium",
		"more confirmed facts omitted"} {
		if !strings.Contains(body, want) {
			t.Fatalf("advisor request lacks %q:\n%s", want, body)
		}
	}
}
