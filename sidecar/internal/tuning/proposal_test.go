package tuning

import (
	"errors"
	"testing"
)

// The model's final answer: typed proposals only, parsed defensively.

func TestParseAnswer_CleanJSON(t *testing.T) {
	a, err := ParseAnswer(`{"proposals":[{"type":"index_create",` +
		`"ddl":"CREATE INDEX CONCURRENTLY orders_customer_idx ON public.orders (customer_id)",` +
		`"rationale":"seq scan","evidence":["S1","R2"],"expected_change_pct":-40,` +
		`"target_queryids":[101]}],"notes":"one index"}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(a.Proposals) != 1 || a.Notes != "one index" {
		t.Fatalf("answer = %+v", a)
	}
	p := a.Proposals[0]
	if p.Type != ProposeIndexCreate || p.ExpectedChangePct == nil ||
		*p.ExpectedChangePct != -40 || len(p.Evidence) != 2 ||
		len(p.TargetQueryIDs) != 1 || p.TargetQueryIDs[0] != 101 {
		t.Fatalf("proposal = %+v", p)
	}
}

func TestParseAnswer_FencedAndWrapped(t *testing.T) {
	for name, raw := range map[string]string{
		"json fence":  "```json\n{\"proposals\":[{\"type\":\"guc\",\"name\":\"work_mem\"}]}\n```",
		"bare fence":  "```\n{\"proposals\":[{\"type\":\"guc\",\"name\":\"work_mem\"}]}\n```",
		"prose first": "Here is my answer:\n{\"proposals\":[{\"type\":\"guc\",\"name\":\"work_mem\"}]}",
		"bare array":  `[{"type":"guc","name":"work_mem"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			a, err := ParseAnswer(raw)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(a.Proposals) != 1 || a.Proposals[0].Name != "work_mem" {
				t.Fatalf("answer = %+v", a)
			}
		})
	}
}

func TestParseAnswer_NoChangeIsAnEmptyList(t *testing.T) {
	a, err := ParseAnswer(`{"proposals":[],"notes":"nothing to do"}`)
	if err != nil || len(a.Proposals) != 0 || a.Notes != "nothing to do" {
		t.Fatalf("answer %+v err %v", a, err)
	}
}

func TestParseAnswer_EmptyAndMalformed(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  string
		want error
	}{
		"empty":            {"", ErrEmptyAnswer},
		"whitespace":       {"  \n\t ", ErrEmptyAnswer},
		"not json":         {"I think you should add an index.", ErrMalformedAnswer},
		"truncated":        {`{"proposals":[{"type":"guc",`, ErrMalformedAnswer},
		"wrong shape":      {`{"proposals":"none"}`, ErrMalformedAnswer},
		"proposal not obj": {`{"proposals":[42]}`, ErrMalformedAnswer},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseAnswer(tc.raw)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestParseAnswer_LargeQueryIDsKeepEveryDigit(t *testing.T) {
	// pg_stat_statements queryids use the whole int64 range; a float64
	// round trip would corrupt them (lifeos: -3534472208683223210).
	a, err := ParseAnswer(`{"proposals":[{"type":"query_hint",` +
		`"queryid":-3534472208683223210,"hint":"SeqScan(t)",` +
		`"target_queryids":["9223372036854775807",-9223372036854775808]}]}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p := a.Proposals[0]
	if int64(p.QueryID) != -3534472208683223210 {
		t.Fatalf("queryid = %d", p.QueryID)
	}
	if int64(p.TargetQueryIDs[0]) != 9223372036854775807 ||
		int64(p.TargetQueryIDs[1]) != -9223372036854775808 {
		t.Fatalf("targets = %v (string and number forms)", p.TargetQueryIDs)
	}
	if _, err := ParseAnswer(`{"proposals":[{"type":"query_hint","queryid":"12x"}]}`); !errors.Is(
		err, ErrMalformedAnswer) {
		t.Fatalf("a non-numeric queryid is malformed: %v", err)
	}
	if _, err := ParseAnswer(`{"proposals":[{"type":"query_hint","queryid":1.5}]}`); !errors.Is(
		err, ErrMalformedAnswer) {
		t.Fatalf("a fractional queryid is malformed: %v", err)
	}
}

func TestParseAnswer_MissingPredictionIsNil(t *testing.T) {
	a, err := ParseAnswer(`{"proposals":[{"type":"guc","name":"work_mem","value":"64MB"}]}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.Proposals[0].ExpectedChangePct != nil {
		t.Fatal("an absent expected_change_pct must stay nil, not 0")
	}
}

func TestParseAnswer_BoundsTheProposalCount(t *testing.T) {
	raw := `{"proposals":[`
	for i := 0; i < maxProposalsPerAnswer+5; i++ {
		if i > 0 {
			raw += ","
		}
		raw += `{"type":"guc","name":"work_mem"}`
	}
	raw += `]}`
	a, err := ParseAnswer(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(a.Proposals) != maxProposalsPerAnswer {
		t.Fatalf("proposals = %d, want the cap %d", len(a.Proposals), maxProposalsPerAnswer)
	}
}
