package facts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// A model proposal must rest on evidence about its own subject: a cited
// line of the right kind that names an object the subject matches, and
// two distinct such objects for a pattern with '*'. Evidence about
// something else is never grounds, however it is phrased.

// groundingEvidence is the production shape of CollectModelEvidence:
// schema:<ident> summaries, slot:<name> and drop_index:<schema.index>.
func groundingEvidence() []EvidenceItem {
	return []EvidenceItem{
		{ID: "E1", Kind: "catalog", Ref: "schema:test_memory_a1", Detail: "3 tables"},
		{ID: "E2", Kind: "catalog", Ref: "schema:test_memory_b2", Detail: "3 tables"},
		{ID: "E3", Kind: "catalog", Ref: "schema:app_core", Detail: "40 tables"},
		{ID: "E4", Kind: "slot", Ref: "slot:cdc_orders", Detail: "logical, pgoutput"},
		{ID: "E5", Kind: "action_log", Ref: "drop_index:public.idx_thesis_allocation_run",
			Detail: "dropped by pg_sage 8 times"},
		{ID: "E6", Kind: "catalog", Ref: `schema:"Test Memory"`, Detail: "2 tables"},
		{ID: "E7", Kind: "catalog", Ref: "schema:public", Detail: "12 tables"},
		{ID: "E8", Kind: "slot", Ref: "slot:cdc_billing", Detail: "logical, pgoutput"},
	}
}

// unparseableEvidence are refs that name no object: they are ignored as
// grounds, never trusted.
func unparseableEvidence() []EvidenceItem {
	return []EvidenceItem{
		{ID: "U1", Kind: "catalog", Ref: "schemas:test_memory_*", Detail: "old shape"},
		{ID: "U2", Kind: "catalog", Ref: "schema:", Detail: "empty"},
		{ID: "U3", Kind: "catalog", Ref: `schema:"unterminated`, Detail: "bad quote"},
		{ID: "U4", Kind: "catalog", Ref: "schema:test_memory_*", Detail: "wildcard ref"},
		{ID: "U5", Kind: "action_log", Ref: "drop_index:idx_thesis_allocation_run",
			Detail: "unqualified index"},
		{ID: "U6", Kind: "slot", Ref: "slot:*", Detail: "every slot"},
		{ID: "U7", Kind: "catalog", Ref: "test_memory_c3", Detail: "no kind prefix"},
		{ID: "U8", Kind: "catalog", Ref: "schema:a.b.c", Detail: "three parts"},
	}
}

func allGroundingEvidence() []EvidenceItem {
	return append(groundingEvidence(), unparseableEvidence()...)
}

func modelItemJSON(typ, kind, subject, value string, ids ...string) string {
	cited := make([]string, 0, len(ids))
	for _, id := range ids {
		cited = append(cited, fmt.Sprintf("%q", id))
	}
	if value == "" {
		value = "{}"
	}
	return fmt.Sprintf(`{"type":%q,"subject_kind":%q,"subject":%q,"value":%s,`+
		`"evidence":[%s]}`, typ, kind, subject, value, strings.Join(cited, ","))
}

func parseOne(t *testing.T, item string) (Proposal, error) {
	t.Helper()
	got, rejected, err := ParseModelProposals("["+item+"]", allGroundingEvidence(), bindNow)
	if err != nil {
		t.Fatalf("parse %s: %v", item, err)
	}
	if len(got)+len(rejected) != 1 {
		t.Fatalf("%s: %d kept, %d rejected", item, len(got), len(rejected))
	}
	if len(rejected) == 1 {
		return Proposal{}, rejected[0]
	}
	return got[0], nil
}

func TestModelProposalsRejectUngroundedItems(t *testing.T) {
	window := `{"kind":"maintenance","window":"sun 02:00-04:00 UTC"}`
	cases := map[string]struct {
		item, why string
	}{
		"append_only citing a schema summary": {modelItemJSON("append_only", "table",
			"app.t0", "", "E3"), "no table evidence"},
		"table_window citing a schema summary": {modelItemJSON("table_window", "table",
			"app_core.jobs", window, "E3"), "no table evidence"},
		"table ownership citing an index drop": {modelItemJSON("owned_by_app_migrations",
			"table", "public.thesis_allocation_run", "", "E5"), "no table evidence"},
		"wildcard schema citing one schema": {modelItemJSON("test_fixture", "schema",
			"app_*", "", "E3"), "app_*"},
		"wildcard citing two refs, one unrelated": {modelItemJSON("test_fixture",
			"schema", "test_memory_*", "", "E1", "E3"), "test_memory_*"},
		"wildcard citing one ref twice": {modelItemJSON("test_fixture", "schema",
			"test_memory_*", "", "E1", "E1"), "test_memory_*"},
		"schema fact citing an unrelated schema": {modelItemJSON("test_fixture", "schema",
			"test_memory_a1", "", "E3"), "test_memory_a1"},
		"schema fact citing an index in it": {modelItemJSON("owned_by_app_migrations",
			"schema", "public", "", "E5"), "public"},
		"slot fact citing a schema": {modelItemJSON("slot_consumer", "slot", "cdc_orders",
			`{"consumer":"debezium"}`, "E1"), "cdc_orders"},
		"slot fact citing another slot": {modelItemJSON("slot_consumer", "slot",
			"cdc_orders", `{"consumer":"debezium"}`, "E8"), "cdc_orders"},
		"wildcard slot citing one slot": {modelItemJSON("slot_consumer", "slot", "cdc_*",
			`{"consumer":"debezium"}`, "E4"), "cdc_*"},
		"index fact citing another index": {modelItemJSON("owned_by_app_migrations",
			"index", "public.idx_orders_created", "", "E5"), "idx_orders_created"},
		"index fact citing its schema": {modelItemJSON("owned_by_app_migrations", "index",
			"public.idx_thesis_allocation_run", "", "E7"), "idx_thesis_allocation_run"},
	}
	for name, c := range cases {
		_, err := parseOne(t, c.item)
		if !errors.Is(err, ErrUngrounded) || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%s: %v, want ErrUngrounded naming %q", name, err, c.why)
		}
		if errors.Is(err, ErrNoEvidence) || errors.Is(err, ErrInvalidKind) {
			t.Errorf("%s: ungrounded must be its own error: %v", name, err)
		}
	}
}

func TestModelProposalsKeepGroundedItems(t *testing.T) {
	cases := map[string]struct {
		item, subject string
		citations     int
	}{
		"wildcard citing two matching schemas": {modelItemJSON("test_fixture", "schema",
			"test_memory_*", "", "E1", "E2"), "test_memory_*", 2},
		"wildcard with an extra unrelated citation": {modelItemJSON("test_fixture",
			"schema", "test_memory_*", "", "E1", "E3", "E2"), "test_memory_*", 3},
		"slot citing its slot": {modelItemJSON("slot_consumer", "slot", "cdc_orders",
			`{"consumer":"debezium"}`, "E4"), "cdc_orders", 1},
		"wildcard slot citing two slots": {modelItemJSON("slot_consumer", "slot", "cdc_*",
			`{"consumer":"debezium"}`, "E4", "E8"), "cdc_*", 2},
		"index citing its drop": {modelItemJSON("owned_by_app_migrations", "index",
			"public.idx_thesis_allocation_run", "", "E5"),
			"public.idx_thesis_allocation_run", 1},
		"exact schema citing its summary": {modelItemJSON("owned_by_app_migrations",
			"schema", "app_core", "", "E3"), "app_core", 1},
		"unquoted subject folds to the summary": {modelItemJSON("test_fixture", "schema",
			"TEST_MEMORY_A1", "", "E1"), "test_memory_a1", 1},
		"quoted schema citing its quoted summary": {modelItemJSON("test_fixture", "schema",
			`"Test Memory"`, "", "E6"), `"Test Memory"`, 1},
		"an unparseable citation beside a grounded one": {modelItemJSON("test_fixture",
			"schema", "test_memory_a1", "", "U1", "E1", "U3"), "test_memory_a1", 3},
	}
	for name, c := range cases {
		p, err := parseOne(t, c.item)
		if err != nil {
			t.Errorf("%s: rejected: %v", name, err)
			continue
		}
		if p.Subject != c.subject || len(p.Evidence) != c.citations ||
			p.Source != SourceModel {
			t.Errorf("%s: kept %+v, want subject %s with %d citations", name, p, c.subject,
				c.citations)
		}
	}
}

func TestModelProposalsIgnoreUnparseableRefs(t *testing.T) {
	cases := map[string]string{
		"old schemas: shape": modelItemJSON("test_fixture", "schema", "test_memory_*", "",
			"U1", "E1"),
		"empty schema ref": modelItemJSON("test_fixture", "schema", "test_memory_a1", "",
			"U2"),
		"unterminated quote": modelItemJSON("test_fixture", "schema", "unterminated", "",
			"U3"),
		"a wildcard ref is not an object": modelItemJSON("test_fixture", "schema",
			"test_memory_*", "", "U4", "E1"),
		"an unqualified index matches no schema": modelItemJSON("owned_by_app_migrations",
			"index", "public.idx_thesis_allocation_run", "", "U5"),
		"slot:* is not this slot": modelItemJSON("slot_consumer", "slot", "cdc_orders",
			`{"consumer":"debezium"}`, "U6"),
		"a ref without a kind": modelItemJSON("test_fixture", "schema", "test_memory_c3",
			"", "U7"),
		"a three-part schema ref": modelItemJSON("test_fixture", "schema", "a", "", "U8"),
	}
	for name, item := range cases {
		if _, err := parseOne(t, item); !errors.Is(err, ErrUngrounded) {
			t.Errorf("%s: %v, want ErrUngrounded", name, err)
		}
	}
}

func TestModelProposalsKeepEvidenceErrorsFirst(t *testing.T) {
	cases := map[string]struct {
		item string
		want error
	}{
		"no citations": {modelItemJSON("append_only", "table", "app.t0", ""),
			ErrNoEvidence},
		"only unknown ids": {modelItemJSON("append_only", "table", "app.t0", "", "E99"),
			ErrNoEvidence},
		"wrong kind for type": {modelItemJSON("append_only", "schema", "app", "", "E3"),
			ErrInvalidKind},
		"protected wildcard": {modelItemJSON("test_fixture", "schema", "s*", "", "E3"),
			ErrProtectedSubject},
		"unknown fact type": {modelItemJSON("owned_by_magic", "schema", "app", "", "E3"),
			ErrInvalidType},
		"invalid slot subject": {modelItemJSON("slot_consumer", "slot", "CDC",
			`{"consumer":"x"}`, "E4"), ErrInvalidSubject},
	}
	for name, c := range cases {
		_, err := parseOne(t, c.item)
		if !errors.Is(err, c.want) || errors.Is(err, ErrUngrounded) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	// Nil evidence: nothing the model cites exists.
	_, rejected, err := ParseModelProposals("["+modelItemJSON("test_fixture", "schema",
		"test_memory_a1", "", "E1")+"]", nil, bindNow)
	if err != nil || len(rejected) != 1 || !errors.Is(rejected[0], ErrNoEvidence) {
		t.Fatalf("nil evidence: %v %v", rejected, err)
	}
}

func TestGroundedOnProposalsDirectly(t *testing.T) {
	slot := Proposal{Type: TypeSlotConsumer, Kind: KindSlot, Subject: "cdc_orders",
		Source: SourceModel, Evidence: []Citation{{Kind: "slot", Ref: " slot:cdc_orders "}}}
	if err := grounded(slot); err != nil {
		t.Fatalf("a padded slot ref still names the slot: %v", err)
	}
	slot.Evidence = nil
	if err := grounded(slot); !errors.Is(err, ErrUngrounded) {
		t.Fatalf("no citations is ungrounded here: %v", err)
	}
	schema := Proposal{Type: TypeTestFixture, Kind: KindSchema, Subject: "test_*",
		Source: SourceModel, Evidence: []Citation{{Ref: "schema:test_a"},
			{Ref: "schema:TEST_A"}, {Ref: `schema:"test_a"`}}}
	if err := grounded(schema); !errors.Is(err, ErrUngrounded) {
		t.Fatalf("three spellings of one schema are one object: %v", err)
	}
	schema.Evidence = append(schema.Evidence, Citation{Ref: "schema:test_b"})
	if err := grounded(schema); err != nil {
		t.Fatalf("two distinct schemas ground a wildcard: %v", err)
	}
	if err := grounded(Proposal{Type: TypeTestFixture, Kind: KindSchema,
		Subject: "bad ident!", Evidence: schema.Evidence}); !errors.Is(err,
		ErrInvalidSubject) {
		t.Fatalf("an invalid subject is reported, not grounded: %v", err)
	}
}

type cannedChatter struct{ reply string }

func (c cannedChatter) Chat(context.Context, string, string, int) (string, int, error) {
	return c.reply, 1, nil
}

func TestModelProposerLogsUngroundedDrops(t *testing.T) {
	raw := "[" + modelItemJSON("append_only", "table", "app.t0", "", "E3") + "," +
		modelItemJSON("test_fixture", "schema", "test_memory_*", "", "E1", "E2") + "]"
	var logs []string
	m := NewModelProposer(cannedChatter{reply: raw}).WithLog(
		func(level, format string, args ...any) {
			logs = append(logs, level+" "+fmt.Sprintf(format, args...))
		})
	got, err := m.Propose(context.Background(), groundingEvidence())
	if err != nil || len(got) != 1 || got[0].Subject != "test_memory_*" {
		t.Fatalf("proposals %+v, %v", got, err)
	}
	if len(logs) != 1 || !strings.HasPrefix(logs[0], "INFO facts: model proposal dropped") ||
		!strings.Contains(logs[0], `append_only "app.t0"`) ||
		!strings.Contains(logs[0], "no table evidence") {
		t.Fatalf("logs %q", logs)
	}
}

func TestModelPromptAsksForGroundedCitations(t *testing.T) {
	for _, want := range []string{"its own subject", "at least two", "no table facts"} {
		if !strings.Contains(modelSystemPrompt, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
}

func TestParseModelProposalsGroundsConcurrently(t *testing.T) {
	// ParseModelProposals is stateless; parallel calls must agree.
	raw := "[" + modelItemJSON("test_fixture", "schema", "test_memory_*", "", "E1", "E2") +
		"," + modelItemJSON("append_only", "table", "app.t0", "", "E3") + "]"
	var wg sync.WaitGroup
	errs := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, rejected, err := ParseModelProposals(raw, groundingEvidence(), bindNow)
			if err != nil || len(got) != 1 || len(rejected) != 1 ||
				!errors.Is(rejected[0], ErrUngrounded) {
				errs <- fmt.Sprintf("%+v %v %v", got, rejected, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
