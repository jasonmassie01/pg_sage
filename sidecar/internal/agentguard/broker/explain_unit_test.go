package broker

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Explain (security-02) shares agent_query's gate: these paths are decided
// before the statement is parsed or a connection is opened, so they need
// neither cgo nor a server. Plans themselves are in explain_db_test.go.
// No concurrent access tests here: nothing below reaches shared state but
// the in-memory audit, which TestQueryConcurrentAndEvict covers.

func boundDeps() Deps {
	deps := fakeDeps()
	deps.Targets = fakeTargets{"db": {Name: "db", DatabaseID: "00000000-0000-0000-0000-" +
		"000000000001"}}
	return deps
}

func TestExplainWithoutPrincipalIsUnsponsored(t *testing.T) {
	deps := boundDeps()
	b := newFakeBroker(t, deps)
	res, err := b.Explain(context.Background(), ExplainRequest{Database: "db",
		SQL: "SELECT 1"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if res.Verdict != VerdictBlocked || res.ReasonCode != "agent_unsponsored" || res.Fix == "" {
		t.Errorf("result = %+v, want blocked agent_unsponsored with a fix", res)
	}
	if len(res.PlanJSON) != 0 {
		t.Errorf("an unsponsored caller got a plan: %s", res.PlanJSON)
	}
	if len(deps.Audit.(*memAudit).all()) != 0 || deps.Logins.(*fakeLogins).calls != 0 {
		t.Error("a caller with no principal was audited or logged in")
	}
}

func TestExplainScopeErrors(t *testing.T) {
	b := newFakeBroker(t, boundDeps())
	ctx := withAgent(context.Background(), testPrincipal(), []string{"other"})
	if _, err := b.Explain(ctx, ExplainRequest{Database: "db", SQL: "SELECT 1"}); !errors.Is(
		err, ErrNotPermitted) {
		t.Errorf("outside the token's databases: %v, want ErrNotPermitted", err)
	}
	ctx = withAgent(context.Background(), testPrincipal(), nil)
	if _, err := b.Explain(ctx, ExplainRequest{Database: "nope", SQL: "SELECT 1"}); !errors.Is(
		err, ErrUnknownDatabase) {
		t.Errorf("unknown database: %v, want ErrUnknownDatabase", err)
	}
}

func TestExplainInvalidArguments(t *testing.T) {
	b := newFakeBroker(t, boundDeps())
	ctx := withAgent(context.Background(), testPrincipal(), nil)
	long := strings.Repeat("x", maxParamLen+1)
	tooMany := make([]string, MaxParams+1)
	for name, req := range map[string]ExplainRequest{
		"no database":     {SQL: "SELECT 1"},
		"blank sql":       {Database: "db", SQL: "  \n"},
		"oversized sql":   {Database: "db", SQL: "SELECT " + strings.Repeat("1", MaxSQLBytes)},
		"too many params": {Database: "db", SQL: "SELECT 1", Params: tooMany},
		"long param":      {Database: "db", SQL: "SELECT $1", Params: []string{long}},
		"NUL in a param":  {Database: "db", SQL: "SELECT $1", Params: []string{"a\x00b"}},
	} {
		_, err := b.Explain(ctx, req)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: error = %v, want ErrInvalid", name, err)
		}
	}
}

func TestExplainUnboundDatabaseIsBlocked(t *testing.T) {
	deps := fakeDeps()
	deps.Targets = fakeTargets{"db": {Name: "db"}}
	b := newFakeBroker(t, deps)
	ctx := withAgent(context.Background(), testPrincipal(), nil)
	res, err := b.Explain(ctx, ExplainRequest{Database: "db", SQL: "SELECT 1"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if res.Verdict != VerdictBlocked || res.ReasonCode != ReasonDatabaseUnbound {
		t.Errorf("result = %+v, want blocked %s", res, ReasonDatabaseUnbound)
	}
}

func TestExplainForbiddenCharactersAreBlockedAndAudited(t *testing.T) {
	deps := boundDeps()
	audit := deps.Audit.(*memAudit)
	b := newFakeBroker(t, deps)
	ctx := withAgent(context.Background(), testPrincipal(), nil)
	res, err := b.Explain(ctx, ExplainRequest{Database: "db", SQL: "SELECT 1 ‮-- x"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if res.Verdict != VerdictBlocked || res.ReasonCode != ReasonForbiddenCharacters {
		t.Errorf("result = %+v, want blocked %s", res, ReasonForbiddenCharacters)
	}
	recs := audit.all()
	if len(recs) != 1 || recs[0].Reason != ReasonForbiddenCharacters ||
		recs[0].TaskID != "task-1" || recs[0].PrincipalID != testPrincipal().ID {
		t.Errorf("audit = %+v, want one attributed blocked row", recs)
	}
}

func TestExplainWithoutAuditReturnsNothing(t *testing.T) {
	deps := boundDeps()
	deps.Audit.(*memAudit).fail = errAuditDown
	b := newFakeBroker(t, deps)
	ctx := withAgent(context.Background(), testPrincipal(), nil)
	res, err := b.Explain(ctx, ExplainRequest{Database: "db", SQL: "SELECT 1 ‮"})
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("error = %v, want ErrUnavailable when the audit cannot be written", err)
	}
	if res.Verdict != "" || len(res.PlanJSON) != 0 {
		t.Errorf("result = %+v, want nothing without an audit row", res)
	}
}

func TestExplainEnvelopeDiffersFromQuery(t *testing.T) {
	q := envelopeHash("agp_x", "db1", "SELECT 1", nil)
	e := envelopeHash("agp_x", "db1", explainEnvelopeStatement("SELECT 1"), nil)
	if q == e || len(e) != 64 {
		t.Errorf("explain envelope %q must differ from the query envelope %q", e, q)
	}
}

func TestExplainExecuteArguments(t *testing.T) {
	for name, c := range map[string]struct {
		params  [][]byte
		n       int
		want    string
		generic bool
	}{
		"all bound":     {[][]byte{[]byte("7"), []byte("o'k")}, 2, `'7', 'o''k'`, false},
		"none given":    {nil, 2, "NULL, NULL", true},
		"some unbound":  {[][]byte{[]byte("7")}, 3, "'7', NULL, NULL", true},
		"explicit NULL": {[][]byte{nil}, 1, "NULL", true},
		"backslash":     {[][]byte{[]byte(`a\'`)}, 1, `'a\'''`, false},
	} {
		got, generic := executeArguments(c.params, c.n)
		if got != c.want || generic != c.generic {
			t.Errorf("%s: arguments = %q (generic %v), want %q (%v)", name, got, generic,
				c.want, c.generic)
		}
	}
}
