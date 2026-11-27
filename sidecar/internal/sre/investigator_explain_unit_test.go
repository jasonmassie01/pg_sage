package sre

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The explainer's configuration and failure typing without a server.

func TestStatementExplainer_UnconfiguredIsAnError(t *testing.T) {
	cases := map[string]*StatementExplainer{
		"nil explainer":  nil,
		"nil runner":     NewStatementExplainer(nil, nil),
		"runner no pool": NewStatementExplainer(probes.NewRunner(nil, probes.Catalog(), nil), nil),
	}
	for name, ex := range cases {
		res := ex.ExplainStatement(context.Background(), 42)
		if res.Status != probes.StatusError || res.Reason != "not_configured" ||
			res.ProbeID != ExplainProbeID {
			t.Errorf("%s: explain = %+v, want error not_configured", name, res)
		}
	}
}

func TestExplainFailure_TypesTheServersAnswer(t *testing.T) {
	cases := []struct {
		err            error
		status         probes.Status
		reason, errSub string
	}{
		{&pgconn.PgError{Code: "42501", Message: "permission denied for table t"},
			probes.StatusNoPrivilege, ReasonMissingSelect, "permission denied"},
		{fmt.Errorf("explain: %w", &pgconn.PgError{Code: "42501", Message: "denied"}),
			probes.StatusNoPrivilege, ReasonMissingSelect, "denied"},
		{&pgconn.PgError{Code: "57014", Message: "canceling statement"},
			probes.StatusError, "statement_timeout", "canceling"},
		{&pgconn.PgError{Code: "55P03", Message: "lock timeout"},
			probes.StatusError, "lock_timeout", "lock timeout"},
		{errors.New("boom"), probes.StatusError, "query_failed", "boom"},
		{context.DeadlineExceeded, probes.StatusError, "deadline_exceeded", "deadline"},
	}
	for _, c := range cases {
		res := explainFailure(probes.Result{ProbeID: ExplainProbeID,
			Rows: []probes.Row{{"x": 1}}}, c.err)
		if res.Status != c.status || res.Reason != c.reason || res.Rows != nil ||
			!strings.Contains(strings.ToLower(res.Error), c.errSub) {
			t.Errorf("explainFailure(%v) = %+v, want %s %s", c.err, res, c.status,
				c.reason)
		}
	}
}
