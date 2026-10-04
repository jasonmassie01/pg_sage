package facts

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func citation(ref string) Citation {
	return Citation{Kind: "catalog", Ref: ref, Detail: "observed",
		ObservedAt: time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC)}
}

func validProposal() Proposal {
	return Proposal{Type: TypeAppMigrations, Kind: KindIndex,
		Subject: "PUBLIC.idx_thesis_allocation_run", Source: SourceDetector,
		ProposedBy: "detector:app_managed_index", Evidence: []Citation{citation("action_log:7")}}
}

func TestValidateNormalizesAValidProposal(t *testing.T) {
	got, err := Validate(validProposal())
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "public.idx_thesis_allocation_run" || got.Type != TypeAppMigrations ||
		len(got.Evidence) != 1 || got.Value == nil {
		t.Fatalf("normalized = %+v", got)
	}
}

func TestValidateTypesAndKinds(t *testing.T) {
	allowed := map[Type][]Kind{
		TypeAppMigrations: {KindIndex, KindTable, KindSchema},
		TypeTestFixture:   {KindSchema},
		TypeSlotConsumer:  {KindSlot},
		TypeAppendOnly:    {KindTable},
		TypeTableWindow:   {KindTable},
	}
	subjects := map[Kind]string{KindIndex: "app.idx_a", KindTable: "app.orders",
		KindSchema: "test_*", KindSlot: "debezium_orders"}
	values := map[Type]map[string]string{
		TypeSlotConsumer: {"consumer": "debezium"},
		TypeTableWindow:  {"kind": "batch", "window": "daily 01:00-03:00 UTC"},
	}
	for typ, kinds := range allowed {
		for _, kind := range []Kind{KindIndex, KindTable, KindSchema, KindSlot} {
			p := Proposal{Type: typ, Kind: kind, Subject: subjects[kind],
				Source: SourceOperator, Value: values[typ]}
			_, err := Validate(p)
			want := false
			for _, k := range kinds {
				want = want || k == kind
			}
			if want && err != nil {
				t.Fatalf("%s/%s: %v", typ, kind, err)
			}
			if !want && !errors.Is(err, ErrInvalidKind) {
				t.Fatalf("%s/%s: err = %v, want ErrInvalidKind", typ, kind, err)
			}
		}
	}
	p := validProposal()
	p.Type = "owned_by_magic"
	if _, err := Validate(p); !errors.Is(err, ErrInvalidType) {
		t.Fatalf("unknown type: %v", err)
	}
}

func TestValidateValues(t *testing.T) {
	slot := func(v map[string]string) Proposal {
		return Proposal{Type: TypeSlotConsumer, Kind: KindSlot, Subject: "cdc_orders",
			Source: SourceOperator, Value: v}
	}
	window := func(v map[string]string) Proposal {
		return Proposal{Type: TypeTableWindow, Kind: KindTable, Subject: "app.events",
			Source: SourceOperator, Value: v}
	}
	bad := []Proposal{
		slot(nil), slot(map[string]string{"consumer": "  "}),
		slot(map[string]string{"consumer": strings.Repeat("c", 129)}),
		slot(map[string]string{"consumer": "x", "owner": "y"}),
		window(nil), window(map[string]string{"kind": "lunch", "window": "always"}),
		window(map[string]string{"kind": "batch", "window": "weeknigths"}),
		window(map[string]string{"kind": "maintenance"}),
		{Type: TypeTestFixture, Kind: KindSchema, Subject: "test_*", Source: SourceOperator,
			Value: map[string]string{"drop": "yes"}},
	}
	for _, p := range bad {
		if _, err := Validate(p); !errors.Is(err, ErrInvalidValue) {
			t.Fatalf("%+v: err = %v, want ErrInvalidValue", p.Value, err)
		}
	}
	good := []Proposal{
		slot(map[string]string{"consumer": "debezium (orders CDC)"}),
		window(map[string]string{"kind": "maintenance", "window": "weekends"}),
		{Type: TypeAppMigrations, Kind: KindSchema, Subject: "app", Source: SourceOperator,
			Value: map[string]string{"repo": "github.com/acme/app", "path": "db/migrate"}},
	}
	for _, p := range good {
		if _, err := Validate(p); err != nil {
			t.Fatalf("%+v: %v", p.Value, err)
		}
	}
}

// What the model and the detectors propose must cite evidence; an
// operator's own declaration is its evidence.
func TestValidateRequiresEvidenceFromModelAndDetectors(t *testing.T) {
	for _, source := range []Source{SourceModel, SourceDetector} {
		p := validProposal()
		p.Source = source
		p.Evidence = nil
		if _, err := Validate(p); !errors.Is(err, ErrNoEvidence) {
			t.Fatalf("%s without evidence: %v", source, err)
		}
		p.Evidence = []Citation{{Kind: "catalog"}}
		if _, err := Validate(p); !errors.Is(err, ErrNoEvidence) {
			t.Fatalf("%s with an empty citation: %v", source, err)
		}
	}
	p := validProposal()
	p.Source, p.Evidence = SourceOperator, nil
	if _, err := Validate(p); err != nil {
		t.Fatalf("operator declaration: %v", err)
	}
	p.Source = "intern"
	if _, err := Validate(p); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("unknown source: %v", err)
	}
}

func TestValidateBoundsSizes(t *testing.T) {
	p := validProposal()
	for i := 0; i < 25; i++ {
		p.Evidence = append(p.Evidence, Citation{Kind: "catalog", Ref: "r",
			Detail: strings.Repeat("d", 900)})
	}
	p.Rationale = strings.Repeat("r", 5000)
	p.ProposedBy = strings.Repeat("p", 500)
	got, err := Validate(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Evidence) != maxCitations || len(got.Rationale) > maxRationale ||
		len(got.ProposedBy) > maxProposedBy {
		t.Fatalf("bounds: %d citations, %d rationale, %d proposer", len(got.Evidence),
			len(got.Rationale), len(got.ProposedBy))
	}
	for _, c := range got.Evidence {
		if len(c.Detail) > maxCitationDetail {
			t.Fatalf("citation detail %d bytes", len(c.Detail))
		}
	}
}

func TestValidateRejectsPastExpiryAndProtectedSubjects(t *testing.T) {
	p := validProposal()
	past := time.Now().Add(-time.Hour)
	p.ExpiresAt = &past
	if _, err := Validate(p); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("past expiry: %v", err)
	}
	p = validProposal()
	p.Subject = "sage.idx_findings"
	if _, err := Validate(p); !errors.Is(err, ErrProtectedSubject) {
		t.Fatalf("sage subject: %v", err)
	}
	p = Proposal{Type: TypeTestFixture, Kind: KindSchema, Subject: "s*",
		Source: SourceOperator}
	if _, err := Validate(p); !errors.Is(err, ErrProtectedSubject) {
		t.Fatalf("s* would match sage: %v", err)
	}
}

func TestFactDescribeAndHash(t *testing.T) {
	at := time.Date(2026, 10, 4, 9, 30, 0, 0, time.UTC)
	f := Fact{ID: 12, Type: TypeAppMigrations, Kind: KindIndex, Subject: "public.idx_a",
		Status: StatusConfirmed, DecidedBy: "alice@example.com", DecidedAt: &at}
	if got := f.Describe(); got != "index public.idx_a is owned by the application's "+
		"migrations" {
		t.Fatalf("Describe = %q", got)
	}
	if got := f.Provenance(); got != "fact #12, confirmed by alice@example.com on 2026-10-04" {
		t.Fatalf("Provenance = %q", got)
	}
	for _, c := range []struct {
		f    Fact
		want string
	}{
		{Fact{Type: TypeTestFixture, Kind: KindSchema, Subject: "test_*"},
			"schemas test_* are test fixtures"},
		{Fact{Type: TypeSlotConsumer, Kind: KindSlot, Subject: "cdc_orders",
			Value: map[string]string{"consumer": "debezium"}},
			"replication slot cdc_orders belongs to debezium"},
		{Fact{Type: TypeAppendOnly, Kind: KindTable, Subject: "app.audit_log"},
			"table app.audit_log is append-only (archive)"},
		{Fact{Type: TypeTableWindow, Kind: KindTable, Subject: "app.events",
			Value: map[string]string{"kind": "batch", "window": "daily 01:00-03:00"}},
			"table app.events has a batch window: daily 01:00-03:00"},
	} {
		if got := c.f.Describe(); got != c.want {
			t.Fatalf("Describe = %q, want %q", got, c.want)
		}
	}
	h1 := f.Hash()
	g := f
	g.Value = map[string]string{"repo": "x"}
	if h1 == "" || h1 == g.Hash() {
		t.Fatalf("hash must change with the value: %q %q", h1, g.Hash())
	}
	g = f
	g.Status = StatusProposed
	if h1 == g.Hash() {
		t.Fatal("hash must change with the status")
	}
	g = f
	g.Proposals = 9
	if h1 != g.Hash() {
		t.Fatal("hash must not change with the proposal count")
	}
}
