package classify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/llm"
)

// Model bounds.
const (
	maxModelColumns     = 80
	maxModelSuggestions = 80
	modelMaxTokens      = 2048
	maxModelComment     = 200
)

// Chatter is the model client (llm.Client).
type Chatter interface {
	Chat(ctx context.Context, system, user string, maxTokens int) (string, int, error)
}

// Suggester proposes classes for columns (the model; rules are Suggest).
type Suggester interface {
	Suggest(ctx context.Context, cols []Column) ([]Proposal, error)
}

// ModelSuggester asks the model which columns are pii, secret or
// untrusted_input from schema text only: names, types and comments, never
// row data (spec §6.16). Its reply is untrusted: only columns it was shown,
// only narrowing classes, only ever proposals.
type ModelSuggester struct{ client Chatter }

// NewModelSuggester proposes through client.
func NewModelSuggester(client Chatter) *ModelSuggester {
	return &ModelSuggester{client: client}
}

const modelSystemPrompt = `You classify columns of one PostgreSQL database for pg_sage, ` +
	`an AI DBA, so that AI agents are never granted sensitive columns. From each column's ` +
	`schema, table, name, type and comment, propose a class an operator will confirm:
- secret: credentials, keys, tokens, password hashes, payment card data.
- pii: data that identifies or contacts a person (names, e-mail, phone, address, ` +
	`birth date, government ids, network addresses, health data).
- untrusted_input: free text written by users or outsiders that an agent might read ` +
	`as instructions (comments, messages, reviews, prompts).
Only list columns that clearly belong to a class; omit the rest. Never propose ` +
	`"clean". Use each column's exact schema.table.column. Respond with only a JSON ` +
	`array, no prose:
[{"column": "schema.table.column", "class": "pii", "rationale": "one sentence"}]
Return [] when no column belongs to a class.
`

// Suggest asks the model about at most maxModelColumns columns; without
// columns it does not ask.
func (m *ModelSuggester) Suggest(ctx context.Context, cols []Column) ([]Proposal, error) {
	if len(cols) == 0 {
		return nil, nil
	}
	if len(cols) > maxModelColumns {
		cols = cols[:maxModelColumns]
	}
	lines := make([]string, 0, len(cols))
	for _, c := range cols {
		line := fmt.Sprintf("%s %s", c.QualifiedName(), c.Type)
		if c.Comment != "" {
			line += " -- " + clip(c.Comment, maxModelComment)
		}
		lines = append(lines, line)
	}
	user := "Columns:\n" + llm.UntrustedData("schema_columns", strings.Join(lines, "\n"))
	raw, _, err := m.client.Chat(ctx, modelSystemPrompt+llm.UntrustedDataRule, user,
		modelMaxTokens)
	if err != nil {
		return nil, fmt.Errorf("model column classes: %w", err)
	}
	props, _, err := ParseModelSuggestions(raw, cols)
	if err != nil {
		return nil, err
	}
	return props, nil
}

type modelItem struct {
	Column    string `json:"column"`
	Class     string `json:"class"`
	Rationale string `json:"rationale"`
}

// ParseModelSuggestions reads the model's reply: the valid proposals (one
// per shown column, at most maxModelSuggestions) and why each other item
// was dropped. A reply that is not a JSON array is ErrModelOutput; a blank
// one llm.ErrEmptyResponse.
func ParseModelSuggestions(raw string, cols []Column) ([]Proposal, []error, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil, llm.ErrEmptyResponse
	}
	var items []modelItem
	if err := json.Unmarshal([]byte(llm.StripJSON(raw, llm.JSONArray)), &items); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrModelOutput, err)
	}
	shown := make(map[string]Column, len(cols))
	for _, c := range cols {
		shown[c.QualifiedName()] = c
	}
	props := []Proposal{}
	seen := map[string]bool{}
	var rejected []error
	for _, it := range items {
		p, err := it.proposal(shown)
		if err != nil {
			rejected = append(rejected, err)
			continue
		}
		if seen[it.Column] || len(props) == maxModelSuggestions {
			continue
		}
		seen[it.Column] = true
		props = append(props, p)
	}
	return props, rejected, nil
}

func (it modelItem) proposal(shown map[string]Column) (Proposal, error) {
	class, err := ParseClass(it.Class)
	if err != nil || class == ClassClean {
		return Proposal{}, fmt.Errorf("%s: %w (model may not propose %q)", it.Column,
			ErrInvalidClass, it.Class)
	}
	col, ok := shown[it.Column]
	if !ok {
		return Proposal{}, fmt.Errorf("%w: %q", ErrUnknownColumn, it.Column)
	}
	rationale := clip(strings.TrimSpace(it.Rationale), 500)
	if rationale == "" {
		rationale = "proposed by the model from the column's schema text"
	}
	return Proposal{Column: col, Class: class, Source: SourceModel, ProposedBy: ModelProposer,
		Rationale: rationale, Evidence: []Citation{{Kind: "column", Ref: col.QualifiedName(),
			Detail: "schema text shown to the model: name, type, comment"}}}, nil
}

// clip cuts s to at most n bytes without splitting a UTF-8 sequence.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
