package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/sre"
)

// Roadmap 2.1: GET /api/v1/databases/{db}/investigations/{id}/transcript
// serves the tool-calling investigator's transcript (plan, each tool call
// with its redacted result and digest, citations, outcome). Viewers get
// the default-deny redaction of the replay export; keeping identifiers is
// an operator opt-in.

// transcriptModel is a fake OpenAI-compatible model: it reads the lock
// graph once, then agrees, citing it.
func transcriptModel(t *testing.T) *llm.Client {
	t.Helper()
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		n++
		k := n
		mu.Unlock()
		name, args := sre.ToolRunProbe, `{"probe":"lock_graph","args":{}}`
		if k > 1 {
			name, args = sre.ToolSubmit, `{"outcome":"agree","claims":[]}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{
			"finish_reason": "stop", "message": map[string]any{"role": "assistant",
				"content": "", "tool_calls": []map[string]any{{"id": "c", "type": "function",
					"function": map[string]any{"name": name, "arguments": args}}}}}},
			"usage": map[string]int{"total_tokens": 200}})
	}))
	t.Cleanup(srv.Close)
	return llm.New(&config.LLMConfig{Enabled: true, Endpoint: srv.URL, APIKey: "k",
		Model: "m", TimeoutSeconds: 5, TokenBudgetDaily: 1_000_000},
		func(string, string, ...any) {})
}

// sreInvestigatorInstance registers a database whose one lock
// investigation ran the tool-calling investigator.
func sreInvestigatorInstance(t *testing.T, mgr *fleet.DatabaseManager,
	name string) sre.Investigation {
	t.Helper()
	pool := surfacePool(t)
	ctx := context.Background()
	limits := sre.DefaultLimits()
	limits.DatabaseDailyTokens, limits.DeploymentDailyTokens = 1_000_000, 1_000_000
	st, err := sre.NewPostgresStore(pool, limits)
	if err != nil {
		t.Fatal(err)
	}
	cc := sre.DefaultCoordinatorConfig(fmt.Sprintf("api-inv:%s:%d", name,
		time.Now().UnixNano()))
	coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: st,
		Runner: lockChainRunner{}, Config: cc, Model: transcriptModel(t),
		Notices: &sre.OnceLog{}, Investigator: &sre.InvestigatorConfig{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coord.Bind(ctx); err != nil {
		t.Fatal(err)
	}
	inv, _, err := coord.Start(ctx, sre.Trigger{CaseID: "incident:" + name + ":lock:1",
		Kind: sre.TriggerLock, Subject: "incident 1", IdempotencyKey: "incident:1"})
	if err != nil || coord.Investigate(ctx, inv.ID) != nil {
		t.Fatalf("investigation: %v", err)
	}
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name, Pool: pool,
		Status: &fleet.InstanceStatus{}, Investigations: sre.NewService(name, coord, st)})
	return inv
}

func transcriptPath(db string, id sre.UUID, query string) string {
	return "/api/v1/databases/" + db + "/investigations/" + string(id) + "/transcript" +
		query
}

func TestTranscriptAPI_ViewerReadsTheRedactedTranscript(t *testing.T) {
	mgr := fleet.NewManager(config.DefaultConfig())
	inv := sreInvestigatorInstance(t, mgr, "orders")
	code, body, ctype := sreCall(t, sreRouter(t, mgr, testViewerUser()), "GET",
		transcriptPath("orders", inv.ID, ""))
	var view struct {
		Schema string `json:"schema"`
		Plan   string `json:"plan"`
		Steps  []struct {
			Tool       string         `json:"tool"`
			EvidenceID string         `json:"evidence_id"`
			Digest     string         `json:"digest"`
			Result     map[string]any `json:"result"`
		} `json:"steps"`
		IdentifiersKept bool `json:"identifiers_kept"`
	}
	if code != 200 || !strings.Contains(ctype, "application/json") ||
		json.Unmarshal([]byte(body), &view) != nil {
		t.Fatalf("transcript = %d %s", code, body)
	}
	if view.Schema != sre.TranscriptSchema || view.Plan != "narrow" || view.IdentifiersKept ||
		len(view.Steps) == 0 || view.Steps[0].Tool != sre.ToolRunProbe ||
		view.Steps[0].Digest == "" || view.Steps[0].Result == nil {
		t.Fatalf("view = %+v", view)
	}
	if strings.Contains(body, "public.orders") {
		t.Fatalf("the viewer's transcript leaks an identifier: %s", body)
	}
}

func TestTranscriptAPI_KeepingIdentifiersIsForOperators(t *testing.T) {
	mgr := fleet.NewManager(config.DefaultConfig())
	inv := sreInvestigatorInstance(t, mgr, "orders")
	path := transcriptPath("orders", inv.ID, "?keep_identifiers=true")
	if code, _, _ := sreCall(t, sreRouter(t, mgr, testViewerUser()), "GET",
		path); code != 403 {
		t.Fatalf("viewer keep_identifiers = %d, want 403", code)
	}
	code, body, _ := sreCall(t, sreRouter(t, mgr, testOperatorUser()), "GET", path)
	if code != 200 || !strings.Contains(body, "public.orders") ||
		!strings.Contains(body, `"identifiers_kept":true`) {
		t.Fatalf("operator keep_identifiers = %d %s", code, body)
	}
}

func TestTranscriptAPI_Errors(t *testing.T) {
	mgr := fleet.NewManager(config.DefaultConfig())
	inv := sreInvestigatorInstance(t, mgr, "orders")
	plain := sreInstance(t, mgr, "billing")
	h := sreRouter(t, mgr, testOperatorUser())
	cases := map[string]struct {
		path string
		code int
		want string
	}{
		"bad flag":      {transcriptPath("orders", inv.ID, "?keep_identifiers=maybe"), 400, ""},
		"unknown id":    {transcriptPath("orders", sre.NewUUID(), ""), 404, "not_found"},
		"other db":      {transcriptPath("billing", inv.ID, ""), 404, "not_found"},
		"unknown db":    {transcriptPath("nope", inv.ID, ""), 404, ""},
		"no transcript": {transcriptPath("billing", plain.ID, ""), 404, "no_transcript"},
		"invalid id":    {"/api/v1/databases/orders/investigations/x/transcript", 400, ""},
	}
	for name, tc := range cases {
		code, body, _ := sreCall(t, h, "GET", tc.path)
		if code != tc.code || !strings.Contains(body, tc.want) {
			t.Errorf("%s: %d %s, want %d %s", name, code, body, tc.code, tc.want)
		}
	}
	if code, _, _ := sreCall(t, sreRouter(t, mgr, nil), "GET",
		transcriptPath("orders", inv.ID, "")); code != 401 {
		t.Fatalf("anonymous = %d, want 401", code)
	}
}
