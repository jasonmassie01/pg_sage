package facts

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// evid-03a: the owner brief is a non-binding fact about the whole database
// (subject kind database, subject "*") that carries what telemetry lacks:
// who owns the database, its peak windows, growth and retention. It is
// prompt context only and never narrows or widens what the gate allows.
//
// No concurrent access tests here: these functions are pure; the store's
// concurrent declarations are in owner_brief_db_test.go.

func briefProposal(value map[string]string) Proposal {
	return Proposal{Type: TypeOwnerBrief, Kind: KindDatabase, Subject: "*", Value: value,
		Source: SourceOperator, ProposedBy: "alice@example.com"}
}

func TestOwnerBriefValidate_HappyPath(t *testing.T) {
	p, err := Validate(briefProposal(map[string]string{
		"owner": "  Payments team ", "contact": "#payments-oncall",
		"peak_windows": "weekdays 09:00-17:00 America/Chicago; sat 10:00-14:00",
		"growth":       "about 20 GB a month", "growth_pct_month": "4.5",
		"retention": "orders kept 7 years", "note": "black friday is the peak",
	}))
	if err != nil {
		t.Fatalf("valid brief rejected: %v", err)
	}
	if p.Subject != "*" || p.Kind != KindDatabase || p.Type != TypeOwnerBrief {
		t.Fatalf("normalized proposal = %+v", p)
	}
	if p.Value["owner"] != "Payments team" || p.Value["growth_pct_month"] != "4.5" ||
		len(p.Value) != 7 {
		t.Fatalf("value = %v, want 7 trimmed keys", p.Value)
	}
}

func TestOwnerBriefValidate_InvalidInput(t *testing.T) {
	long := strings.Repeat("x", maxValueText+1)
	cases := []struct {
		name string
		p    Proposal
		want error
	}{
		{"bad peak window", briefProposal(map[string]string{"peak_windows": "lunchtime"}),
			ErrInvalidValue},
		{"one bad window of two", briefProposal(map[string]string{
			"peak_windows": "weekdays 09:00-17:00; 25:00-26:00"}), ErrInvalidValue},
		{"negative growth", briefProposal(map[string]string{"growth_pct_month": "-1"}),
			ErrInvalidValue},
		{"growth over 1000", briefProposal(map[string]string{
			"growth_pct_month": "1000.5"}), ErrInvalidValue},
		{"growth not a number", briefProposal(map[string]string{
			"growth_pct_month": "lots"}), ErrInvalidValue},
		{"growth NaN", briefProposal(map[string]string{"growth_pct_month": "NaN"}),
			ErrInvalidValue},
		{"unknown key", briefProposal(map[string]string{"owner": "a", "tables": "b"}),
			ErrInvalidValue},
		{"301-character value", briefProposal(map[string]string{"note": long}),
			ErrInvalidValue},
		{"subject other than *", Proposal{Type: TypeOwnerBrief, Kind: KindDatabase,
			Subject: "public", Value: map[string]string{"owner": "a"},
			Source: SourceOperator}, ErrInvalidSubject},
		{"subject a pattern", Proposal{Type: TypeOwnerBrief, Kind: KindDatabase,
			Subject: "**", Value: map[string]string{"owner": "a"},
			Source: SourceOperator}, ErrInvalidSubject},
		{"kind table", Proposal{Type: TypeOwnerBrief, Kind: KindTable,
			Subject: "public.orders", Value: map[string]string{"owner": "a"},
			Source: SourceOperator}, ErrInvalidKind},
		{"database kind on another type", Proposal{Type: TypeAppendOnly,
			Kind: KindDatabase, Subject: "*", Source: SourceOperator}, ErrInvalidKind},
	}
	for _, c := range cases {
		if _, err := Validate(c.p); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
}

func TestOwnerBriefValidate_EmptyIsRejected(t *testing.T) {
	for name, value := range map[string]map[string]string{
		"nil": nil, "empty": {}, "blank values": {"owner": "  ", "note": ""},
	} {
		if _, err := Validate(briefProposal(value)); !errors.Is(err, ErrInvalidValue) {
			t.Errorf("%s: err = %v, want ErrInvalidValue", name, err)
		}
	}
}

func TestOwnerBriefValidate_Boundaries(t *testing.T) {
	exact := strings.Repeat("y", maxValueText)
	p, err := Validate(briefProposal(map[string]string{"note": exact}))
	if err != nil || p.Value["note"] != exact {
		t.Fatalf("a %d-character value: %v (kept %d)", maxValueText, err,
			len(p.Value["note"]))
	}
	for _, g := range []string{"0", "1000", "0.0", "999.99"} {
		if _, err := Validate(briefProposal(map[string]string{
			"growth_pct_month": g})); err != nil {
			t.Errorf("growth_pct_month %s rejected: %v", g, err)
		}
	}
	// A blank key next to a filled one is dropped, not stored empty.
	p, err = Validate(briefProposal(map[string]string{"owner": "dba", "contact": " "}))
	if err != nil || len(p.Value) != 1 || p.Value["owner"] != "dba" {
		t.Fatalf("blank key kept: %v %v", p.Value, err)
	}
}

func TestOwnerBriefPattern(t *testing.T) {
	p, err := ParsePattern(KindDatabase, " * ")
	if err != nil || p.Kind != KindDatabase || p.String() != "*" {
		t.Fatalf("ParsePattern(database, *) = %+v, %v", p, err)
	}
	for _, ref := range []ObjectRef{{Kind: KindTable, Schema: "public", Name: "orders"},
		{Kind: KindRelation, Name: "orders"}, {Kind: KindSlot, AllSlots: true},
		{Kind: KindSchema, Schema: "public"}, {Kind: KindIndex, Schema: "a", Name: "b"}} {
		if p.Matches(ref) {
			t.Errorf("the database pattern matched %s; a brief names no object", ref)
		}
	}
	if _, err := ParsePattern(KindDatabase, "orders"); !errors.Is(err, ErrInvalidSubject) {
		t.Fatalf("subject orders: %v, want ErrInvalidSubject", err)
	}
	if _, err := CoverageOf(t.Context(), nil, KindDatabase, "*"); !errors.Is(err,
		ErrInvalidKind) {
		t.Fatalf("coverage of a database subject: %v, want ErrInvalidKind", err)
	}
}

func confirmedBrief(id int64, value map[string]string) Fact {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	return Fact{ID: id, Type: TypeOwnerBrief, Kind: KindDatabase, Subject: "*",
		Value: value, Source: SourceOperator, Status: StatusConfirmed,
		DecidedBy: "alice@example.com", DecidedAt: &at}
}

func TestOwnerBriefDescribe(t *testing.T) {
	f := confirmedBrief(3, map[string]string{"owner": "Payments", "contact": "#pay",
		"peak_windows": "weekdays 09:00-17:00", "growth": "20 GB/month",
		"growth_pct_month": "5", "retention": "7 years", "note": "quiet in August"})
	want := "owner brief: owner Payments, contact #pay, peak weekdays 09:00-17:00, " +
		"growth 20 GB/month, growth 5%/month, retention 7 years, note quiet in August"
	if got := f.Describe(); got != want {
		t.Fatalf("Describe() =\n%q\nwant\n%q", got, want)
	}
	if got := confirmedBrief(4, map[string]string{"owner": "dba"}).Describe(); got !=
		"owner brief: owner dba" {
		t.Fatalf("Describe() of an owner-only brief = %q", got)
	}
}

func TestOwnerBriefPromptLines_FirstAndAlwaysRelevant(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	slot := Fact{ID: 1, Type: TypeSlotConsumer, Kind: KindSlot, Subject: "cdc",
		Value: map[string]string{"consumer": "debezium"}, Status: StatusConfirmed}
	brief := confirmedBrief(9, map[string]string{"owner": "Payments",
		"peak_windows": "weekdays 09:00-17:00"})
	lines := PromptLines([]Fact{slot, brief}, now)
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "- fact #9 (operator-confirmed): "+
		"owner brief: owner Payments, peak weekdays 09:00-17:00") {
		t.Fatalf("lines = %q, want the brief first", lines)
	}
	if !strings.Contains(lines[0], "context, not a rule") {
		t.Fatalf("brief line %q lacks its guidance", lines[0])
	}
	lines = PromptLines([]Fact{slot, brief}, now, "public.orders")
	if len(lines) != 1 || !strings.Contains(lines[0], "fact #9") {
		t.Fatalf("lines for public.orders = %q, want the brief (it is about the database)",
			lines)
	}
	for _, st := range []Status{StatusProposed, StatusRejected, StatusExpired} {
		b := brief
		b.Status = st
		if got := PromptLines([]Fact{b}, now); len(got) != 0 {
			t.Errorf("a %s brief reached the prompt: %q", st, got)
		}
	}
}

func TestOwnerBriefPromptLines_LongBriefIsCutWithNote(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	value := map[string]string{}
	for _, k := range []string{"owner", "contact", "growth", "retention", "note"} {
		value[k] = strings.Repeat(k[:1], maxValueText)
	}
	lines := PromptLines([]Fact{confirmedBrief(2, value)}, now)
	if len(lines) != 1 {
		t.Fatalf("lines = %q", lines)
	}
	if len(lines[0]) != maxPromptLine || !strings.HasSuffix(lines[0], briefCutNote) {
		t.Fatalf("line is %d characters ending %q, want %d ending %q", len(lines[0]),
			lines[0][len(lines[0])-30:], maxPromptLine, briefCutNote)
	}
	if !strings.Contains(lines[0], "context, not a rule") {
		t.Fatalf("the cut dropped the guidance: %q", lines[0])
	}
}

func TestOwnerBriefNeverBinds(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	refs := []ObjectRef{{Kind: KindTable, Schema: "public", Name: "orders"},
		{Kind: KindRelation, Name: "orders"}, {Kind: KindSlot, AllSlots: true},
		{Kind: KindSchema, Schema: "public"}}
	reqs := []Request{
		{ActionType: "create_index_concurrently", SQL: "CREATE INDEX i ON public.orders (a)",
			Now: now},
		{ActionType: "retention_delete", SQL: "DELETE FROM public.orders", Now: now},
		{ActionType: "vacuum", SQL: "VACUUM FULL public.orders", Now: now},
		{ActionType: "drop_slot", SQL: "SELECT pg_drop_replication_slot('cdc')", Now: now},
		{ActionType: "alter_table", SQL: "ALTER TABLE public.orders SET (fillfactor=90)",
			OperatorApproved: true, Now: now},
	}
	for _, st := range []Status{StatusProposed, StatusConfirmed, StatusRejected,
		StatusExpired} {
		b := confirmedBrief(5, map[string]string{"owner": "dba",
			"peak_windows": "always"})
		b.Status = st
		for _, req := range reqs {
			if got := Bind([]Fact{b}, refs, req); len(got) != 0 {
				t.Errorf("%s brief bound %s: %+v", st, req.ActionType, got)
			}
		}
		if route, applies := routeFor(b, classify(reqs[0]), reqs[0]); applies ||
			route != "" {
			t.Errorf("routeFor(%s brief) = %q, %v; want not applicable", st, route, applies)
		}
	}
}
