package agenttools

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/agentguard/readapi"
	"github.com/pg-sage/sidecar/internal/explain"
)

// security-10: explain errors an agent or the MCP client sees never quote
// a value; the operator log keeps the full server text.
// No state transition tests: explainError holds no state.

const leakedSSN = "123-45-6789"

type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (c *logCapture) log(level, format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, level+" "+fmt.Sprintf(format, args...))
}

func (c *logCapture) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.lines, "\n")
}

func ssnCastError() *pgconn.PgError {
	return &pgconn.PgError{Severity: "ERROR", Code: "22P02",
		Message: `invalid input syntax for type integer: "` + leakedSSN + `"`}
}

func TestExplainError_WithholdsADataExceptionAndLogsIt(t *testing.T) {
	logs := &logCapture{}
	tools := New(nil, Options{Log: logs.log})
	got := tools.explainError(fmt.Errorf("execute explain: %w", ssnCastError()))
	if got == nil || strings.Contains(got.Error(), leakedSSN) {
		t.Fatalf("agent-facing error quotes the value: %v", got)
	}
	var pgErr *pgconn.PgError
	if !errors.As(got, &pgErr) || pgErr.Code != "22P02" {
		t.Fatalf("SQLSTATE not kept: %v", got)
	}
	if !strings.HasPrefix(got.Error(), "explain query: ") {
		t.Errorf("error %q lost the tool's context", got.Error())
	}
	if out := logs.text(); !strings.Contains(out, leakedSSN) ||
		!strings.HasPrefix(out, "INFO ") {
		t.Errorf("operator log %q lacks the full server text at INFO", out)
	}
}

func TestExplainError_SentinelMappingsUnchanged(t *testing.T) {
	logs := &logCapture{}
	tools := New(nil, Options{Log: logs.log})
	cases := []struct {
		err  error
		want error
	}{
		{fmt.Errorf("%w: id 7", explain.ErrUnknownQueryID), ErrNotFound},
		{fmt.Errorf("%w: no extension", explain.ErrStatementsUnavailable), ErrUnavailable},
		{fmt.Errorf("%w: write refused", explain.ErrExplainInvalidRequest), ErrInvalid},
	}
	for _, c := range cases {
		got := tools.explainError(c.err)
		if !errors.Is(got, c.want) || !strings.Contains(got.Error(), c.err.Error()) {
			t.Errorf("explainError(%v) = %v, want %v with the original text", c.err, got,
				c.want)
		}
	}
	if logs.text() != "" {
		t.Errorf("non-withheld errors were logged: %q", logs.text())
	}
}

func TestExplainError_OtherServerErrorsKeepTheirText(t *testing.T) {
	tools := New(nil, Options{})
	missing := &pgconn.PgError{Code: "42P01", Message: `relation "nope" does not exist`}
	got := tools.explainError(missing)
	if !strings.Contains(got.Error(), `relation "nope" does not exist`) {
		t.Errorf("42P01 text withheld: %v", got)
	}
}

func TestExplainError_NilToolsDoNotPanic(t *testing.T) {
	var tools *Tools
	got := tools.explainError(ssnCastError())
	if got == nil || strings.Contains(got.Error(), leakedSSN) {
		t.Fatalf("nil tools leaked or dropped the error: %v", got)
	}
}

func TestPlannerError_WithholdsADataException(t *testing.T) {
	logs := &logCapture{}
	tools := New(nil, Options{Log: logs.log})
	got := tools.plannerError(fmt.Errorf("plan: %w", ssnCastError()))
	if got == nil || strings.Contains(got.Error(), leakedSSN) {
		t.Fatalf("agent-facing planner error quotes the value: %v", got)
	}
	if !strings.Contains(logs.text(), leakedSSN) {
		t.Errorf("operator log %q lacks the full server text", logs.text())
	}
	if !errors.Is(tools.plannerError(fmt.Errorf("%w: down", readapi.ErrUnavailable)),
		ErrUnavailable) {
		t.Error("readapi.ErrUnavailable no longer maps to ErrUnavailable")
	}
}

func TestExplainError_ConcurrentCallsLogEachFailure(t *testing.T) {
	logs := &logCapture{}
	tools := New(nil, Options{Log: logs.log})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := tools.explainError(ssnCastError()); strings.Contains(err.Error(),
				leakedSSN) {
				t.Errorf("concurrent call leaked: %v", err)
			}
		}()
	}
	wg.Wait()
	if n := strings.Count(logs.text(), leakedSSN); n != 16 {
		t.Errorf("operator log has %d entries with the full text, want 16", n)
	}
}
