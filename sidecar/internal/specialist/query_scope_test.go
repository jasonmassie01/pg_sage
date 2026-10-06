package specialist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Optional query_id / query_hash (contract revision 1.1.0): a caller may
// scope an investigation to one statement. A plan-regression investigation
// is then about that statement (its probe reads only it, its diagnosis is
// of it); for every other family the probes still read the whole database
// and the result names which evidence mentions the statement. The scope is
// validated strictly, a hash is resolved through pg_stat_statements with a
// parameter (never caller text in SQL), and the result echoes it only to
// the caller that sent it.

const queryHashOK = "5e884898da28047151d0e56f8dc6292773603d0d6aabbdd62a11ef721d1542d8"

func decodeValidOpen(t *testing.T, body string) (OpenRequest, error) {
	t.Helper()
	req, err := DecodeOpenRequest(strings.NewReader(body))
	if err != nil {
		return req, err
	}
	return req, req.Validate(created.Add(5 * 60e9))
}

func TestQueryID_Validation(t *testing.T) {
	ok := map[string]string{
		`"42"`: "42", `42`: "42", `"-1"`: "-1", `"9223372036854775807"`: "9223372036854775807",
		`"-9223372036854775808"`: "-9223372036854775808",
		// Above 2^53: an integer is read exactly, never through float64.
		`9007199254740993`: "9007199254740993", `null`: "",
	}
	for raw, want := range ok {
		req, err := decodeValidOpen(t, `{"symptom":{"summary":"slow"},"query_id":`+raw+`}`)
		if err != nil || string(req.QueryID) != want {
			t.Errorf("%s: %q (%v), want %q", raw, req.QueryID, err, want)
		}
	}
	bad := []string{`"0"`, `0`, `"-0"`, `"007"`, `"+42"`, `" 42"`, `"4 2"`, `""`,
		`"9223372036854775808"`, `9223372036854775808`, `"-9223372036854775809"`, `1.5`,
		`1e3`, `true`, `{}`, `[1]`, `"42; DROP TABLE sage.findings"`, `"0x2a"`,
		`"123456789012345678901"`}
	for _, raw := range bad {
		_, err := decodeValidOpen(t, `{"symptom":{"summary":"slow"},"query_id":`+raw+`}`)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("query_id %s: %v, want invalid_request", raw, err)
		}
	}
}

func TestQueryHash_Validation(t *testing.T) {
	req, err := decodeValidOpen(t, `{"symptom":{"summary":"slow"},"query_hash":"`+queryHashOK+`"}`)
	if err != nil || req.QueryHash != queryHashOK {
		t.Fatalf("valid hash: %q %v", req.QueryHash, err)
	}
	for _, h := range []string{strings.ToUpper(queryHashOK), queryHashOK[:63],
		queryHashOK + "0", strings.Repeat("g", 64), "' OR 1=1 --",
		strings.Repeat("a", 63) + "\n"} {
		raw, _ := json.Marshal(h)
		_, err := decodeValidOpen(t, `{"symptom":{"summary":"slow"},"query_hash":`+string(raw)+`}`)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("query_hash %q: %v, want invalid_request", h, err)
		}
	}
	_, err = decodeValidOpen(t, `{"symptom":{"summary":"slow"},"query_hash":42}`)
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("a numeric query_hash: %v", err)
	}
}

// Validate also guards requests built in code (MCP, adapters).
func TestQueryID_ValidateRejectsProgrammaticGarbage(t *testing.T) {
	for _, q := range []QueryID{"abc", "0", "1.0", "99999999999999999999"} {
		r := OpenRequest{Symptom: &Symptom{Summary: "s"}, QueryID: q}
		if err := r.Validate(created); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: %v", q, err)
		}
	}
}

func planQueryReq(qid string) OpenRequest {
	r := symptomReq("plan_regression")
	r.QueryID = QueryID(qid)
	return r
}

func TestOpen_PlanRegressionQueryScopesTheInvestigation(t *testing.T) {
	h := newHarness(t, DefaultLimits())
	resp, err := h.svc.Open(context.Background(), reader, "orders", planQueryReq("-123"))
	if err != nil {
		t.Fatal(err)
	}
	if len(h.orders.started) != 1 || h.orders.started[0].Subject != "queryid -123" ||
		h.orders.started[0].Kind != sre.TriggerPlan {
		t.Fatalf("the plan investigation is about the statement: %+v", h.orders.started)
	}
	q := resp.QueryScope
	if q == nil || q.QueryID != "-123" || q.Applied != QueryAppliedProbes || q.QueryHash != "" {
		t.Fatalf("open echoes the scope: %+v", q)
	}
	recs := h.store.all()
	if len(recs) != 1 || recs[0].Query == nil || recs[0].Query.QueryID != "-123" ||
		recs[0].Query.Applied != QueryAppliedProbes {
		t.Fatalf("the caller's record keeps the scope: %+v", recs)
	}
	if len(h.orders.hashCalls) != 0 {
		t.Fatalf("no hash, no resolution: %v", h.orders.hashCalls)
	}
}

func TestOpen_OtherFamiliesScopeTheResultEvidence(t *testing.T) {
	h := newHarness(t, DefaultLimits())
	req := symptomReq("lock_blocking")
	req.QueryID = "77"
	resp, err := h.svc.Open(context.Background(), reader, "orders", req)
	if err != nil {
		t.Fatal(err)
	}
	if h.orders.started[0].Subject != externalSubject {
		t.Fatalf("a lock investigation is not about one statement: %+v", h.orders.started)
	}
	if resp.QueryScope == nil || resp.QueryScope.Applied != QueryAppliedEvidence {
		t.Fatalf("scope %+v", resp.QueryScope)
	}
	noScope, err := h.svc.Open(context.Background(), proposer, "orders",
		symptomReq("wal_retention"))
	if err != nil || noScope.QueryScope != nil {
		t.Fatalf("a request without a query has no scope: %+v %v", noScope.QueryScope, err)
	}
}

func TestOpen_DistinctStatementsOpenDistinctInvestigations(t *testing.T) {
	h := newHarness(t, Limits{WritesPerMinute: 100, ReadsPerMinute: 100,
		MaxOpenPerIdentity: 20, MaxOpenTotal: 20})
	var wg sync.WaitGroup
	ids := make([]string, 8)
	errs := make([]error, 8)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := h.svc.Open(context.Background(), reader, "orders",
				planQueryReq(fmt.Sprint(1000+i%4)))
			ids[i], errs[i] = resp.Investigation.ID, err
		}(i)
	}
	wg.Wait()
	distinct := map[string]bool{}
	for i, id := range ids {
		if errs[i] != nil {
			t.Fatalf("open %d: %v", i, errs[i])
		}
		distinct[id] = true
	}
	if len(distinct) != 4 {
		t.Fatalf("4 statements, %d investigations: %v", len(distinct), ids)
	}
}

func TestOpen_QueryHashResolvesToTheStatement(t *testing.T) {
	h := newHarness(t, DefaultLimits())
	h.orders.hashes = map[string][]int64{queryHashOK: {-9001}}
	req := symptomReq("plan_regression")
	req.QueryHash = queryHashOK
	resp, err := h.svc.Open(context.Background(), reader, "orders", req)
	if err != nil {
		t.Fatal(err)
	}
	if h.orders.started[0].Subject != "queryid -9001" || resp.QueryScope.QueryID != "-9001" ||
		resp.QueryScope.QueryHash != queryHashOK {
		t.Fatalf("hash resolution: %+v %+v", h.orders.started, resp.QueryScope)
	}
	if len(h.orders.hashCalls) != 1 || h.orders.hashCalls[0] != queryHashOK {
		t.Fatalf("resolver calls %v", h.orders.hashCalls)
	}
	// Both given and consistent.
	req.QueryID = "-9001"
	req.Family = "temp_file_explosion"
	if _, err := h.svc.Open(context.Background(), reader, "orders", req); err != nil {
		t.Fatalf("consistent id and hash: %v", err)
	}
}

func TestOpen_QueryHashFailures(t *testing.T) {
	h := newHarness(t, DefaultLimits())
	h.orders.hashes = map[string][]int64{queryHashOK: {1, 2}}
	other := strings.Repeat("0", 64)
	cases := []struct {
		name string
		hash string
		id   QueryID
		err  error
		want error
	}{
		{"no statement", other, "", nil, ErrInvalid},
		{"ambiguous", queryHashOK, "", nil, ErrInvalid},
		{"mismatch", queryHashOK, "3", nil, ErrInvalid},
		{"pg_stat_statements unavailable", other, "", fmt.Errorf("%w: pg_stat_statements "+
			"is not installed", ErrUnavailable), ErrUnavailable},
		{"connection lost", other, "", errors.New("conn closed"), ErrUnavailable},
	}
	for _, c := range cases {
		h.orders.hashErr = c.err
		req := symptomReq("plan_regression")
		req.QueryHash, req.QueryID = c.hash, c.id
		_, err := h.svc.Open(context.Background(), reader, "orders", req)
		if codeOf(err) != c.want {
			t.Errorf("%s: %v (code %s)", c.name, err, codeOf(err).code)
		}
		if c.err != nil && !strings.Contains(err.Error(), c.err.Error()) {
			t.Errorf("%s: the cause is lost: %v", c.name, err)
		}
	}
	if len(h.orders.started) != 0 {
		t.Fatalf("nothing opens when the scope cannot be resolved: %+v", h.orders.started)
	}
	// A hash naming two statements is resolved by the query_id among them.
	h.orders.hashErr = nil
	req := symptomReq("plan_regression")
	req.QueryHash, req.QueryID = queryHashOK, "2"
	if _, err := h.svc.Open(context.Background(), reader, "orders", req); err != nil {
		t.Fatalf("query_id picks among the hash's statements: %v", err)
	}
}

func TestOpen_ScopeIsCheckedBeforeTheBackend(t *testing.T) {
	h := newHarness(t, DefaultLimits())
	req := symptomReq("plan_regression")
	req.QueryHash = queryHashOK
	noRead := Identity{TokenID: "t", Scopes: []string{}, Databases: []string{"orders"}}
	if _, err := h.svc.Open(context.Background(), noRead, "orders", req); !errors.Is(err,
		ErrScope) {
		t.Fatalf("scope first: %v", err)
	}
	if _, err := h.svc.Open(context.Background(), reader, "billing", req); !errors.Is(err,
		ErrDatabaseNotPermitted) {
		t.Fatalf("database first: %v", err)
	}
	if len(h.orders.hashCalls)+len(h.other.hashCalls) != 0 {
		t.Fatal("the hash reached a backend before admission")
	}
}

func TestOpen_AttachChecksTheStatement(t *testing.T) {
	h := newHarness(t, DefaultLimits())
	first, err := h.svc.Open(context.Background(), reader, "orders", planQueryReq("5"))
	if err != nil {
		t.Fatal(err)
	}
	attach := OpenRequest{Attach: &Attach{InvestigationID: first.Investigation.ID},
		QueryID: "6"}
	if _, err := h.svc.Open(context.Background(), proposer, "orders", attach); !errors.Is(err,
		ErrInvalid) {
		t.Fatalf("attaching another statement's plan investigation: %v", err)
	}
	attach.QueryID = "5"
	resp, err := h.svc.Open(context.Background(), proposer, "orders", attach)
	if err != nil || resp.Match != "investigation_id" || resp.QueryScope == nil ||
		resp.QueryScope.Applied != QueryAppliedProbes {
		t.Fatalf("same statement attaches: %+v %v", resp, err)
	}
}

func TestOpen_WindowAttachMatchesTheStatement(t *testing.T) {
	h := newHarness(t, DefaultLimits())
	if _, err := h.svc.Open(context.Background(), reader, "orders", planQueryReq("5")); err != nil {
		t.Fatal(err)
	}
	req := planQueryReq("6")
	req.Window = &Window{Start: created}
	resp, err := h.svc.Open(context.Background(), proposer, "orders", req)
	if err != nil || resp.Match != "new" || !resp.Created {
		t.Fatalf("another statement's investigation in the window is not attached: %+v %v",
			resp, err)
	}
	req = planQueryReq("5")
	req.Window = &Window{Start: created}
	resp, err = h.svc.Open(context.Background(), proposer, "orders", req)
	if err != nil || resp.Match != "window" {
		t.Fatalf("the same statement attaches in the window: %+v %v", resp, err)
	}
}

func TestOpen_IdempotencyKeyIsScopedByStatement(t *testing.T) {
	a := symptomReq("lock_blocking")
	a.IdempotencyKey = "k1"
	b := a
	b.QueryID = "9"
	if idempotencyKey(reader, a) == idempotencyKey(reader, b) {
		t.Fatal("one key for two statements would return another statement's investigation")
	}
	if idempotencyKey(reader, a) != idempotencyKey(reader, symptomReqWithKey("k1")) {
		t.Fatal("a v1 request's key must not change")
	}
}

func symptomReqWithKey(key string) OpenRequest {
	r := symptomReq("lock_blocking")
	r.IdempotencyKey = key
	return r
}

func planDetail(id sre.UUID, qid string) sre.Detail {
	ev := []sre.EvidenceView{
		evidenceView(ev1, "plan_regressions", "ok", `{"rows":[{"queryid":`+qid+
			`,"before_mean_ms":1,"after_mean_ms":9},{"queryid":31337,"before_mean_ms":1}]}`),
		evidenceView(ev2, "top_statements", "ok", `{"rows":[{"query_id":`+qid+`,"calls":5}]}`),
		evidenceView(ev3, "sage_actions", "ok", `{"rows":[{"queryid":31337}]}`),
	}
	root := sre.HypothesisRecord{Revision: 1, Ordinal: 1, Family: "plan_regression",
		Node: "plan_flip_regression", Label: "Plan flip", Subject: "queryid " + qid,
		Status: sre.HypothesisRoot, Confidence: 0.9,
		Support: []sre.Fact{{EvidenceID: ev1, Text: "plan_hash changed"}}}
	return sre.Detail{Database: "orders", Investigation: sre.Investigation{ID: id,
		TriggerKind: sre.TriggerPlan, Subject: "queryid " + qid, State: sre.StateConcluded,
		CreatedAt: created, UpdatedAt: created, ConcludedAt: created,
		Summary: sre.Summary{Family: "plan_regression", Conclusive: true,
			Root: "plan_flip_regression"}},
		Hypotheses: []sre.HypothesisRecord{root}, Evidence: ev, ChainVerified: true}
}

func TestResult_QueryScopeNamesTheEvidence(t *testing.T) {
	h := newHarness(t, DefaultLimits())
	resp, err := h.svc.Open(context.Background(), reader, "orders", planQueryReq("4242"))
	if err != nil {
		t.Fatal(err)
	}
	id := sre.UUID(resp.Investigation.ID)
	h.orders.put(planDetail(id, "4242"))
	r, err := h.svc.Result(context.Background(), reader, "orders", string(id))
	if err != nil {
		t.Fatal(err)
	}
	q := r.QueryScope
	if q == nil || q.QueryID != "4242" || q.Applied != QueryAppliedProbes || !q.RootMatches ||
		len(q.EvidenceIDs) != 2 || q.EvidenceIDs[0] != string(ev1) ||
		q.EvidenceIDs[1] != string(ev2) {
		t.Fatalf("result scope: %+v", q)
	}
	// Another caller of the same investigation sent no query: no scope.
	other, err := h.svc.Result(context.Background(), proposer, "orders", string(id))
	if err != nil || other.QueryScope != nil {
		t.Fatalf("the scope is echoed only to its caller: %+v %v", other.QueryScope, err)
	}
}

func TestQueryScope_RootOfAnotherStatementDoesNotMatch(t *testing.T) {
	d := planDetail(inv, "31337")
	rec := &Record{Query: &QueryScope{QueryID: "4242", Applied: QueryAppliedEvidence}}
	r := MapResult(Snapshot{Detail: d, Record: rec}, MapOptions{KeepIdentifiers: true,
		Now: created})
	q := r.QueryScope
	if q == nil || q.RootMatches || q.QueryID != "4242" || len(q.EvidenceIDs) != 0 ||
		q.EvidenceIDs == nil {
		t.Fatalf("a root about another statement: %+v", q)
	}
	// A plan-shaped subject that only shares a prefix is not the statement.
	d = planDetail(inv, "42420")
	rec.Query.QueryID = "4242"
	if r := MapResult(Snapshot{Detail: d, Record: rec}, MapOptions{Now: created}); r.QueryScope.
		RootMatches || len(r.QueryScope.EvidenceIDs) != 0 {
		t.Fatalf("prefix match: %+v", r.QueryScope)
	}
}

func TestQueryScope_AbsentWithoutAScopedRecord(t *testing.T) {
	r := MapResult(Snapshot{Detail: planDetail(inv, "1"), Record: &Record{}},
		MapOptions{Now: created})
	if r.QueryScope != nil {
		t.Fatalf("no query in the record: %+v", r.QueryScope)
	}
}
