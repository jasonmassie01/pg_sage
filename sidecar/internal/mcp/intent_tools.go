package mcp

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

// The agent-native intent tools (F6): policy, ledger and intent-level
// change requests. Mutations go through the standing policy gate.

// legacyDatabaseIDTools take the integer database_id the server fills in
// from the resolved database.
var legacyDatabaseIDTools = map[string]bool{"get_policy": true,
	"propose_policy_change": true, "request_change": true, "optimize_query": true,
	"apply_migration": true, "ensure_fk_indexes": true, "declare_table_contract": true,
	"register_consumer": true, "set_maintenance_policy": true,
	"get_guarantee_status": true, "get_value": true, "get_ledger": true}

func (s *Server) callIntentTool(ctx context.Context, name string,
	arguments json.RawMessage) (any, *rpcError) {
	if failed := validateIntentArguments(name, arguments); failed != nil {
		return nil, failed
	}
	var result any
	var err error
	switch name {
	case "get_policy":
		var request PolicyRequest
		if !decodeArguments(arguments, &request) {
			return nil, failure(codeInvalidParams, "invalid arguments")
		}
		result, err = s.backend.GetPolicy(ctx, request)
	case "propose_policy_change":
		var request PolicyProposalRequest
		if !decodeArguments(arguments, &request) || emptyJSON(request.Delta) {
			return nil, failure(codeInvalidParams, "delta is required")
		}
		result, err = s.backend.ProposePolicyChange(ctx, request)
	case "request_change":
		var request ChangeRequest
		if !decodeArguments(arguments, &request) || emptyJSON(request.Intent) {
			return nil, failure(codeInvalidParams, "intent is required")
		}
		result, err = s.backend.RequestChange(ctx, request)
	case "get_ledger":
		var request LedgerRequest
		if !decodeArguments(arguments, &request) {
			return nil, failure(codeInvalidParams, "invalid arguments")
		}
		result, err = s.backend.GetLedger(ctx, request)
	default:
		return s.callNamedIntent(ctx, name, arguments)
	}
	return intentResult(ctx, name, result, err)
}

func (s *Server) callNamedIntent(ctx context.Context, name string,
	arguments json.RawMessage) (any, *rpcError) {
	backend, ok := s.backend.(IntentBackend)
	if !ok {
		return nil, failure(codeInternal, "internal error")
	}
	result, err := backend.RequestIntent(ctx, name, arguments)
	return intentResult(ctx, name, result, err)
}

// migrationTable is a schema-qualified name of plain identifiers; the
// migration planner does not take quoted identifiers.
var migrationTable = regexp.MustCompile(
	`^[A-Za-z_][A-Za-z0-9_$]{0,62}\.[A-Za-z_][A-Za-z0-9_$]{0,62}$`)

const maxMigrationSQL = 20000

// validateMigrationArguments checks apply_migration against its schema:
// a schema-qualified table, the migration SQL and an optional cycle.
func validateMigrationArguments(arguments json.RawMessage) *rpcError {
	var input struct {
		Database   string `json:"database"`
		DatabaseID *int64 `json:"database_id"`
		Table      string `json:"table"`
		SQL        string `json:"sql"`
		Cycle      *int   `json:"cycle"`
	}
	switch {
	case !decodeStrict(arguments, &input):
		return failure(codeInvalidParams, "invalid arguments: apply_migration takes "+
			"table, sql and cycle")
	case !migrationTable.MatchString(input.Table):
		return failure(codeInvalidParams, "invalid arguments: table must be a "+
			"schema-qualified name such as public.orders")
	case strings.TrimSpace(input.SQL) == "" || len(input.SQL) > maxMigrationSQL:
		return failure(codeInvalidParams, "invalid arguments: sql is the migration "+
			"statement (1-20000 characters)")
	case input.Cycle != nil && *input.Cycle < 0:
		return failure(codeInvalidParams, "invalid arguments: cycle must be >= 0")
	}
	return nil
}

// changeIntentSchema types request_change's intent: the kinds
// intentContract can plan, plus that kind's own arguments.
const changeIntentSchema = `{"type":"object","description":"the change: kind is ` +
	`optimize_query, ensure_fk_indexes, apply_migration, declare_table_contract or ` +
	`register_consumer, plus that kind's arguments (such as query_id, schema, table or ` +
	`slot_name)","properties":{"kind":{"type":"string","enum":["optimize_query",` +
	`"ensure_fk_indexes","apply_migration","declare_table_contract",` +
	`"register_consumer"]}},"required":["kind"]}`

func intentTools() []Tool {
	object := func(properties string, required string) json.RawMessage {
		return json.RawMessage(`{"type":"object","properties":` + properties +
			`,"required":` + required + `,"additionalProperties":false}`)
	}
	dbID := `"database_id":{"type":"integer","description":"legacy numeric database id; ` +
		`prefer database"}`
	tools := []Tool{
		{Name: "get_policy", Description: "Use before request_change or optimize_query " +
			"to see the rules pg_sage's policy gate applies to this database. Returns the " +
			"standing policy's version and profile. Read-only: it never changes policy " +
			"or approves anything; propose_policy_change asks a person for a change",
			InputSchema: object(`{`+dbID+`}`, `[]`)},
		{Name: "propose_policy_change", Description: "Propose a policy delta (a dry-run " +
			"proposal a person ratifies)",
			InputSchema: object(`{`+dbID+`,"delta":{"type":"object"},`+
				`"caller_claims":{"type":"object"}}`, `["delta"]`)},
		{Name: "request_change", Description: "Use to ask for a database change by " +
			"intent rather than SQL: intent names a kind plus that kind's arguments. " +
			"pg_sage plans it and its policy gate decides. Returns the gate's decision, " +
			"the evidence_id, the reason and, when the change ran, its outcome. It never " +
			"approves: a change the gate holds waits for a person (see get_inbox)",
			InputSchema: object(`{`+dbID+`,"intent":`+changeIntentSchema+`,`+
				`"caller_claims":{"type":"object"}}`, `["intent"]`)},
		{Name: "optimize_query", Description: "Use when a query is slow: name it by " +
			"query_id or query_text with goal latency. pg_sage plans an index under " +
			"standing policy and its policy gate decides whether it runs. Returns the " +
			"gate's decision, the evidence_id, the reason and the outcome. It never " +
			"approves: a change the gate holds waits for a person (see get_inbox)",
			InputSchema: object(`{`+dbID+`,"query_id":{"type":"integer"},`+
				`"query_text":{"type":"string"},"goal":{"const":"latency"},`+
				`"constraints":{"type":"object"}}`, `["goal"]`)},
	}
	return append(tools, moreIntentTools(object, dbID)...)
}

func moreIntentTools(object func(string, string) json.RawMessage, dbID string) []Tool {
	return []Tool{
		{Name: "apply_migration", Description: "Plan and rehearse an online migration " +
			"(ADD UNIQUE / SET NOT NULL rewritten into expand steps). The policy gate is " +
			"consulted before any clone rehearsal and again before each expand step; " +
			"contract steps are never run",
			InputSchema: object(`{`+dbID+`,"table":{"type":"string","pattern":`+
				`"^[A-Za-z_][A-Za-z0-9_$]{0,62}\\.[A-Za-z_][A-Za-z0-9_$]{0,62}$",`+
				`"description":"schema-qualified table the migration changes"},`+
				`"sql":{"type":"string","minLength":1,"maxLength":20000,`+
				`"description":"the migration statement"},"cycle":{"type":"integer",`+
				`"minimum":0,"description":"deploy cycle of this migration"}}`,
				`["table","sql"]`)},
		{Name: "ensure_fk_indexes", Description: "Ensure foreign keys have supporting indexes",
			InputSchema: object(`{`+dbID+`,"schema":{"type":"string"}}`, `["schema"]`)},
		{Name: "declare_table_contract", Description: "Declare table intent constraints " +
			"(a person's declaration, imported as a confirmed fact). retention needs both " +
			"interval and column (the timestamptz, timestamp or date column whose age " +
			"defines retention); pg_sage never infers the column",
			InputSchema: object(`{`+dbID+`,"table":{"type":"string"},`+
				`"append_only":{"type":"boolean"},`+
				`"retention":{"type":"object","properties":{"interval":{"type":"string"},`+
				`"column":{"type":"string"}},"required":["interval","column"]},`+
				`"expected_pk":{"type":"string"},`+
				`"exemptions":{"type":"array","items":{"type":"string"}}}`, `["table"]`)},
		{Name: "register_consumer", Description: "Protect a replication slot consumer " +
			"(a person's declaration, imported as a confirmed fact)",
			InputSchema: object(`{`+dbID+`,"slot_name":{"type":"string"},`+
				`"owner":{"type":"string"},"consumer_identity":{"type":"string"}}`,
				`["slot_name","owner"]`)},
		{Name: "set_maintenance_policy", Description: "Use to propose a change to how " +
			"pg_sage maintains a scope (a patch to its maintenance policy). Returns the " +
			"proposal_id and the dry-run impact; a person ratifies the proposal. It never " +
			"approves or applies the patch itself",
			InputSchema: object(`{`+dbID+`,"scope":{"type":"object"},"patch":{"type":"object"}}`,
				`["scope","patch"]`)},
		{Name: "get_guarantee_status", Description: "Read machine-readable invariant status",
			InputSchema: object(`{`+dbID+`}`, `[]`)},
		{Name: "get_value", Description: "Use to report what pg_sage has been worth. " +
			"Returns verified DBA-hours saved by period, feature and database, incidents " +
			"avoided, hours still pending and a daily trend; partial and unavailable name " +
			"any source that could not be read. Read-only: it never changes anything",
			InputSchema: object(`{`+dbID+`}`, `[]`)},
		{Name: "get_ledger", Description: "Use to audit what pg_sage's policy gate " +
			"decided. Returns the newest policy decision records (evidence_id, decision, " +
			"feature), narrowed by filter {decision, feature, limit 1-1000, default 100}. " +
			"Read-only and it never approves. It holds decisions, not results: use " +
			"get_outcomes for what an executed change measurably did",
			InputSchema: object(`{`+dbID+`,"filter":{"type":"object"}}`, `[]`)},
	}
}
