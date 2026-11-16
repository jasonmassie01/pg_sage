package tuning

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/verify"
)

// lab-04a, the model-facing edge: the tuning agent calls rehearse. Bad
// arguments are refused under the existing tool rules without a clone; a
// rehearsal's tier-2 record rides on the proposal of the same index.

const rehearseDDL = `{"ddl":"CREATE INDEX CONCURRENTLY orders_customer_idx ON ` +
	`public.orders (customer_id)"}`

func blocks(n int64) *int64 { return &n }

func passingRehearser() *fakeRehearser {
	return &fakeRehearser{res: RehearsalResult{BuildMs: 40, SizeDeltaBytes: 8192,
		ReceiptID: 77, Provider: "dle", Queries: []RehearsedQuery{{QueryID: 101,
			BeforeCost: 900, AfterCost: 9, BeforeBlocks: blocks(1000), AfterBlocks: blocks(20)}}}}
}

func TestRehearseTool_ProposalCarriesTheTierTwoRecord(t *testing.T) {
	h := newHarness(t, toolCall("rehearse", rehearseDDL), indexAnswer(t))
	reh := passingRehearser()
	h.agent.deps.Rehearse = reh
	out := tune(t, h)
	f, ok := findingByCategory(out.Findings, optimizer.OptimizerCategory)
	if !ok {
		t.Fatalf("no index finding: %+v", out.Findings)
	}
	recs, err := verify.DecodeLadderValue(f.Detail["ladder"])
	if err != nil || len(recs) != 1 {
		t.Fatalf("ladder %v (%v)", f.Detail["ladder"], err)
	}
	rec := recs[0]
	if !rec.Passed() || rec.Tier != verify.TierClone || rec.Predicted == nil ||
		*rec.Predicted != -98 || rec.Evidence["receipt_id"] != float64(77) {
		t.Fatalf("tier-2 record = %+v", rec)
	}
	if len(reh.got) != 1 || len(reh.got[0].Statements) == 0 {
		t.Fatalf("one rehearsal of the case statements: %+v", reh.got)
	}
}

func TestRehearseTool_BadArgumentsAreRefusedWithoutAClone(t *testing.T) {
	for name, args := range map[string]string{
		"malformed JSON": `{"ddl":`,
		"markdown fence": "```json\n" + rehearseDDL + "\n```",
		"two statements": `{"ddl":"CREATE INDEX CONCURRENTLY a ON public.orders (status); ` +
			`DROP TABLE public.orders"}`,
		"not an index": `{"ddl":"ALTER SYSTEM SET work_mem = '1GB'"}`,
		"empty":        `{}`,
	} {
		h := newHarness(t)
		reh := passingRehearser()
		h.agent.deps.Rehearse = reh
		tb := ordersToolbox(t, h)
		out := call(t, tb, "rehearse", args)
		if out["error"] == nil || len(reh.got) != 0 {
			t.Fatalf("%s: result %v, rehearsals %d", name, out, len(reh.got))
		}
		// A refused call does not use the cycle's one rehearsal.
		if ok := call(t, tb, "rehearse", rehearseDDL); ok["error"] != nil || len(reh.got) != 1 {
			t.Fatalf("%s: a valid call after a refusal: %v", name, ok)
		}
	}
}

func TestRehearseTool_FailedRehearsalIsRecordedAsSkipped(t *testing.T) {
	h := newHarness(t, toolCall("rehearse", rehearseDDL), indexAnswer(t))
	h.agent.deps.Rehearse = &fakeRehearser{err: errors.New("dle: no free pool")}
	out := tune(t, h)
	f, ok := findingByCategory(out.Findings, optimizer.OptimizerCategory)
	if !ok {
		t.Fatalf("a failed rehearsal still lets the proposal queue: %+v", out.Findings)
	}
	recs, err := verify.DecodeLadderValue(f.Detail["ladder"])
	if err != nil || len(recs) != 1 || recs[0].Result != verify.LadderSkipped ||
		!strings.Contains(recs[0].Skipped[0].Reason, "dle: no free pool") {
		t.Fatalf("ladder %+v (%v)", recs, err)
	}
}

func TestRehearseTool_ModelFailuresMidCycle(t *testing.T) {
	for name, fail := range map[string]func([]llm.Message) (llm.ToolResult, error){
		"empty reply": answer(""),
		"timeout":     failing(context.DeadlineExceeded),
		"rate limit":  failing(llm.ErrRateLimited),
	} {
		h := newHarness(t, toolCall("rehearse", rehearseDDL), fail)
		reh := passingRehearser()
		h.agent.deps.Rehearse = reh
		prev, cur := ordersPair()
		out, err := h.agent.Tune(context.Background(), cur, prev)
		if err != nil {
			t.Fatalf("%s: a model failure degrades the cycle, it does not fail it: %v", name,
				err)
		}
		if len(out.Findings) != 0 || len(reh.got) != 1 || h.model.callCount() != 2 {
			t.Fatalf("%s: findings %d rehearsals %d calls %d", name, len(out.Findings),
				len(reh.got), h.model.callCount())
		}
	}
}

func TestRehearseTool_OfferedOnlyWithARehearser(t *testing.T) {
	h := newHarness(t)
	if slices.Contains(h.agent.ToolNames(), "rehearse") {
		t.Fatal("no clone provider: no rehearse tool")
	}
	h.agent.deps.Rehearse = passingRehearser()
	names := h.agent.ToolNames()
	if !slices.Contains(names, "rehearse") || !slices.Contains(names, "whatif_index") {
		t.Fatalf("tool names = %v", names)
	}
}
