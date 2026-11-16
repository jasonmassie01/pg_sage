package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// Plan cards in chat (plan-03b): the fixture is the approval-card chat
// fixture plus plans stored in its database through the real plan store.

// planResolver accepts every citation, so the fixture's plans validate.
type planResolver struct{}

func (planResolver) FindingExists(context.Context, int64) (bool, error) { return true, nil }

func (planResolver) EvidenceExists(context.Context, string, string) (bool, error) {
	return true, nil
}

func (planResolver) TargetCalls(context.Context, []int64, time.Duration) (int64, error) {
	return 5000, nil
}

// moderateStep is a reversible index create (risk tier moderate).
func moderateStep(ordinal int, table string) changeplan.Step {
	name := fmt.Sprintf("idx_%s_%d", table, ordinal)
	return changeplan.Step{Ordinal: ordinal, Kind: changeplan.KindAction,
		ActionType:  "create_index_concurrently",
		ProposedSQL: "CREATE INDEX CONCURRENTLY " + name + " ON public." + table + " (id)",
		RollbackSQL: "DROP INDEX CONCURRENTLY public." + name, FindingID: ptrTo(int64(11)),
		TargetQueryIDs: []int64{101}, Metric: changeplan.MetricBlocksPerCall,
		ExpectedDirection: changeplan.DirectionDown, ExpectedSource: changeplan.SourceHypoPG,
		DeadlineMinutes: 60}
}

// highStep ends a backend (risk tier high): never approvable from chat.
func highStep(ordinal int) changeplan.Step {
	return changeplan.Step{Ordinal: ordinal, Kind: changeplan.KindAction,
		ActionType: "terminate_backend", ProposedSQL: "SELECT pg_terminate_backend(4242)",
		FindingID: ptrTo(int64(11)), TargetQueryIDs: []int64{101},
		Metric: changeplan.MetricTimePerCall, ExpectedDirection: changeplan.DirectionDown,
		ExpectedSource: changeplan.SourceNone, DeadlineMinutes: 60,
		OnUnverifiable: changeplan.ActionKeep}
}

// createPlan stores a proposed plan of steps in the fixture's database.
func (cf *chatCardFixture) createPlan(t *testing.T, steps ...changeplan.Step) changeplan.Plan {
	t.Helper()
	p, err := changeplan.NewStore(cf.pool).Create(context.Background(), changeplan.Plan{
		Goal: "cut blocks per call on " + cf.table, Origin: changeplan.OriginAgent,
		OriginRef: "agp_chat", CreatedBy: "mcp:token:t1", Status: changeplan.StatusProposed,
		Scope: changeplan.Scope{Objects: []string{"public." + cf.table}}, Steps: steps},
		planResolver{})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	return p
}

// issuePlan mints a plan card on channel for p offering offered.
func (cf *chatCardFixture) issuePlan(t *testing.T, channel int, p changeplan.Plan,
	upTo int, offered ...chatops.PlanChoice) string {
	t.Helper()
	token, err := chatops.NewPlanCardStore(cf.pool).Issue(context.Background(),
		chatops.PlanCardIssue{ChannelID: channel, Database: "orders", PlanID: p.ID,
			PlanHash: p.PlanHash, Title: p.Goal, Offered: offered, UpTo: upTo,
			ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatalf("issue plan card: %v", err)
	}
	return token
}

func (cf *chatCardFixture) planCard(t *testing.T, token string) chatops.PlanCardDelivery {
	t.Helper()
	d, err := chatops.NewPlanCardStore(cf.pool).Lookup(context.Background(), token)
	if err != nil {
		t.Fatalf("plan card: %v", err)
	}
	return d
}

func (cf *chatCardFixture) planDetail(t *testing.T, id int64) changeplan.Detail {
	t.Helper()
	d, err := changeplan.NewStore(cf.pool).Detail(context.Background(), id)
	if err != nil {
		t.Fatalf("plan detail: %v", err)
	}
	return d
}

func (cf *chatCardFixture) telegramPlanPress(t *testing.T, c chatops.PlanChoice,
	token string) (int, map[string]any) {
	t.Helper()
	tgUpdates.mu.Lock()
	tgUpdates.n++
	update := tgUpdates.n
	tgUpdates.mu.Unlock()
	fromID, _ := strconv.ParseInt(cf.suffix, 10, 64)
	raw, _ := json.Marshal(map[string]any{"update_id": update,
		"callback_query": map[string]any{"id": "cb-" + strconv.FormatInt(update, 10),
			"from":    map[string]any{"id": fromID, "username": "bob"},
			"message": map[string]any{"message_id": 9003, "chat": map[string]any{"id": -1001}},
			"data":    chatops.PlanCallbackData(c, token)}})
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/chatops/telegram/%d", cf.tgCh), bytes.NewReader(raw))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "tg-secret_1")
	return serveJSON(cf.h, req)
}

// failingDecisions is the real plan store whose decisions fail.
type failingDecisions struct {
	planService
	err error
}

func (f failingDecisions) ApproveWith(context.Context, int64, changeplan.Decision,
	int) (changeplan.Approval, error) {
	return changeplan.Approval{}, f.err
}

func (f failingDecisions) Reject(context.Context, int64, string, int) error { return f.err }

// failPlanDecisions makes every chat plan decision fail with err.
func failPlanDecisions(t *testing.T, err error) {
	t.Helper()
	restore := chatPlanStores
	chatPlanStores = func(inst *fleet.DatabaseInstance) planService {
		return failingDecisions{planService: restore(inst), err: err}
	}
	t.Cleanup(func() { chatPlanStores = restore })
}
