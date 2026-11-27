package sre

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// security-10: the investigator's EXPLAIN probe result reaches a model
// prompt, so a server error that can quote a value (SQLSTATE classes 22
// and 23) is withheld from it; the SQLSTATE and the typing stay.
// No concurrent access or state tests: failedRead is a pure function.

func TestExplainFailure_WithholdsValueQuotingServerText(t *testing.T) {
	const ssn = "123-45-6789"
	cases := []*pgconn.PgError{
		{Severity: "ERROR", Code: "22P02",
			Message: `invalid input syntax for type integer: "` + ssn + `"`},
		{Severity: "ERROR", Code: "23505", Message: "duplicate key value",
			Detail: "Key (ssn)=(" + ssn + ") already exists."},
	}
	for _, pgErr := range cases {
		res := explainFailure(probes.Result{ProbeID: ExplainProbeID},
			fmt.Errorf("explain: %w", pgErr))
		if strings.Contains(res.Error, ssn) {
			t.Errorf("%s: probe error quotes the value: %q", pgErr.Code, res.Error)
		}
		if !strings.Contains(res.Error, "SQLSTATE "+pgErr.Code) ||
			!strings.Contains(res.Error, "withheld") {
			t.Errorf("%s: probe error %q lacks the SQLSTATE or the withheld note",
				pgErr.Code, res.Error)
		}
		if res.Status != probes.StatusError || res.Reason == "" {
			t.Errorf("%s: typing changed: %s %q", pgErr.Code, res.Status, res.Reason)
		}
	}
}

func TestExplainFailure_UndefinedTableKeepsItsText(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "42P01", Message: `relation "nope" does not exist`}
	res := explainFailure(probes.Result{ProbeID: ExplainProbeID}, pgErr)
	if !strings.Contains(res.Error, `relation "nope" does not exist`) {
		t.Errorf("42P01 text withheld: %q", res.Error)
	}
}
