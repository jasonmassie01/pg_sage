package facts

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
)

// Model proposal bounds.
const (
	maxModelProposals = 10
	maxModelEvidence  = 40
	modelMaxTokens    = 2048
)

// EvidenceItem is one catalog observation shown to the model, cited by ID.
type EvidenceItem struct {
	ID     string
	Kind   string
	Ref    string
	Detail string
}

// Chatter is the model client (llm.Client).
type Chatter interface {
	Chat(ctx context.Context, system, user string, maxTokens int) (string, int, error)
}

// ModelProposer asks the model which facts the evidence supports. Its
// reply is untrusted: every item must cite evidence it was shown and pass
// Validate, and it is only ever a proposal.
type ModelProposer struct {
	client Chatter
	logFn  func(string, string, ...any)
	now    func() time.Time
}

// NewModelProposer proposes through client.
func NewModelProposer(client Chatter) *ModelProposer {
	return &ModelProposer{client: client, logFn: func(string, string, ...any) {},
		now: time.Now}
}

// WithLog sets the logger of rejected items.
func (m *ModelProposer) WithLog(logFn func(string, string, ...any)) *ModelProposer {
	if logFn != nil {
		m.logFn = logFn
	}
	return m
}

const modelSystemPrompt = `You classify objects of one PostgreSQL database for pg_sage, ` +
	`an AI DBA. From the catalog evidence, propose typed facts an operator will confirm ` +
	`or reject. Fact types and their subject kinds:
- owned_by_app_migrations (index | table | schema): the application's schema ` +
	`migrations create and change it; pg_sage must not change it with DDL.
- test_fixture (schema): test-run schemas, not application workload.
- slot_consumer (slot): a replication slot belongs to a named consumer (CDC); value ` +
	`{"consumer": "<name>"}.
- append_only (table): rows are only ever inserted (an archive or log).
- table_window (table): value {"kind": "maintenance"|"batch", "window": "<window>"}.
Subjects: index and table subjects are schema.name, a schema subject is a name, '*' ` +
	`matches any characters. Never propose anything in the sage, pg_catalog or ` +
	`information_schema schemas.
Propose only what the evidence supports, and cite the evidence IDs (E1, E2, ...) each ` +
	`fact rests on. Each fact cites evidence about its own subject: a schema fact its ` +
	`schema: line, a slot fact its slot: line, an index fact its drop_index: line; a ` +
	`subject with '*' cites at least two objects it matches. No table evidence is shown, ` +
	`so propose no table facts. Respond with only a JSON array, no prose:
[{"type": "...", "subject_kind": "...", "subject": "...", "value": {}, ` +
	`"evidence": ["E1"], "rationale": "one sentence"}]
Return [] when nothing is supported.
`

// Propose asks the model about evidence; without evidence it does not ask.
func (m *ModelProposer) Propose(ctx context.Context, evidence []EvidenceItem) (
	[]Proposal, error) {
	if len(evidence) == 0 {
		return nil, nil
	}
	lines := make([]string, 0, len(evidence))
	for _, e := range evidence {
		lines = append(lines, fmt.Sprintf("%s [%s] %s: %s", e.ID, e.Kind, e.Ref, e.Detail))
	}
	user := "Catalog evidence:\n" + llm.UntrustedData("catalog_evidence",
		strings.Join(lines, "\n"))
	pctx := llm.WithPurpose(ctx, "facts") // llm-04: usage ledger label
	raw, _, err := m.client.Chat(pctx, modelSystemPrompt+llm.UntrustedDataRule, user,
		modelMaxTokens)
	if err != nil {
		return nil, fmt.Errorf("model fact proposals: %w", err)
	}
	props, rejected, err := ParseModelProposals(raw, evidence, m.now())
	if err != nil {
		return nil, err
	}
	for _, r := range rejected {
		m.logFn("INFO", "facts: model proposal dropped: %v", r)
	}
	return props, nil
}

type modelItem struct {
	Type        string         `json:"type"`
	SubjectKind string         `json:"subject_kind"`
	Subject     string         `json:"subject"`
	Value       map[string]any `json:"value"`
	Evidence    []string       `json:"evidence"`
	Rationale   string         `json:"rationale"`
}

// ParseModelProposals reads the model's reply: the valid proposals (at
// most maxModelProposals) and why each other item was dropped. A reply
// that is not a JSON array is ErrModelOutput; a blank one
// llm.ErrEmptyResponse.
func ParseModelProposals(raw string, evidence []EvidenceItem, now time.Time) (
	[]Proposal, []error, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil, llm.ErrEmptyResponse
	}
	var items []modelItem
	if err := json.Unmarshal([]byte(llm.StripJSON(raw, llm.JSONArray)), &items); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrModelOutput, err)
	}
	byID := make(map[string]EvidenceItem, len(evidence))
	for _, e := range evidence {
		byID[e.ID] = e
	}
	props := []Proposal{}
	var rejected []error
	for _, item := range items {
		p, err := Validate(item.proposal(byID, now))
		if err == nil {
			err = grounded(p)
		}
		if err != nil {
			rejected = append(rejected, fmt.Errorf("%s %q: %w", item.Type, item.Subject, err))
			continue
		}
		if len(props) < maxModelProposals {
			props = append(props, p)
		}
	}
	return props, rejected, nil
}

func (item modelItem) proposal(byID map[string]EvidenceItem, now time.Time) Proposal {
	p := Proposal{Type: Type(item.Type), Kind: Kind(item.SubjectKind),
		Subject: item.Subject, Source: SourceModel, ProposedBy: "model",
		Rationale: item.Rationale}
	if len(item.Value) > 0 {
		p.Value = make(map[string]string, len(item.Value))
		for k, v := range item.Value {
			p.Value[k] = fmt.Sprint(v)
		}
	}
	for _, id := range item.Evidence {
		if e, ok := byID[id]; ok {
			p.Evidence = append(p.Evidence, Citation{Kind: e.Kind, Ref: e.Ref,
				Detail: e.Detail, ObservedAt: now})
		}
	}
	return p
}

// groundingRefs is the evidence ref prefix that can ground a model fact of
// each subject kind (CollectModelEvidence). No table evidence is shown to
// the model, so a table fact from it is never grounded.
var groundingRefs = map[Kind]string{KindSchema: "schema", KindSlot: "slot",
	KindIndex: "drop_index"}

// grounded requires a model proposal (normalized by Validate) to cite
// evidence about its own subject: a ref of its kind that the subject
// matches, and two distinct such objects when the subject has '*'. A ref
// that names no object is ignored, never trusted.
func grounded(p Proposal) error {
	prefix, ok := groundingRefs[p.Kind]
	if !ok {
		return fmt.Errorf("%w: no %s evidence is collected for model proposals",
			ErrUngrounded, p.Kind)
	}
	pattern, err := ParsePattern(p.Kind, p.Subject)
	if err != nil {
		return err
	}
	matched := map[ObjectRef]bool{}
	for _, c := range p.Evidence {
		if ref, ok := citedRef(prefix, c.Ref); ok && pattern.Matches(ref) {
			matched[ref] = true
		}
	}
	need := 1
	if strings.Contains(p.Subject, "*") {
		need = 2
	}
	if len(matched) < need {
		return fmt.Errorf("%w: %d cited %s: refs match %s, %d needed", ErrUngrounded,
			len(matched), prefix, p.Subject, need)
	}
	return nil
}

// citedRef reads a citation ref "<prefix>:<ident>" as the object it names.
// A ref of another kind, with a wildcard, or not fully qualified names no
// object (an unqualified ref would match every schema).
func citedRef(prefix, raw string) (ObjectRef, bool) {
	kind, ident, ok := strings.Cut(strings.TrimSpace(raw), ":")
	if !ok || kind != prefix || strings.Contains(ident, "*") {
		return ObjectRef{}, false
	}
	if kind == "slot" {
		ident = strings.TrimSpace(ident)
		return ObjectRef{Kind: KindSlot, Name: ident}, slotPattern.MatchString(ident)
	}
	parts, err := splitIdent(strings.TrimSpace(ident))
	if err != nil {
		return ObjectRef{}, false
	}
	switch {
	case kind == "schema" && len(parts) == 1:
		return ObjectRef{Kind: KindSchema, Schema: parts[0].text}, true
	case kind == "drop_index" && len(parts) == 2:
		return ObjectRef{Kind: KindIndex, Schema: parts[0].text, Name: parts[1].text}, true
	}
	return ObjectRef{}, false
}
