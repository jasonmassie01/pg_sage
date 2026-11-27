package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// request_change's typed intent (mcp-01a): a oneOf over the kinds
// intentContract can plan, each branch being that kind's own arguments
// plus kind, with an example. A shape matching no branch is refused
// before any backend sees it.

// changeIntentKinds are the plannable kinds, in their fixed order.
var changeIntentKinds = []string{"optimize_query", "ensure_fk_indexes", "apply_migration",
	"declare_table_contract", "register_consumer"}

// changeIntentExamples are one valid intent of each kind.
var changeIntentExamples = map[string]string{
	"optimize_query":    `{"kind":"optimize_query","query_id":4711,"goal":"latency"}`,
	"ensure_fk_indexes": `{"kind":"ensure_fk_indexes","schema":"public"}`,
	"apply_migration": `{"kind":"apply_migration","table":"public.orders",` +
		`"sql":"ALTER TABLE public.orders ADD CONSTRAINT orders_ref_key UNIQUE (ref)"}`,
	"declare_table_contract": `{"kind":"declare_table_contract","table":"public.events",` +
		`"append_only":true,"retention":{"interval":"90 days","column":"created_at"}}`,
	"register_consumer": `{"kind":"register_consumer","slot_name":"debezium_orders",` +
		`"owner":"data-platform"}`,
}

// defaultIntentKind is the example offered when the kind is unknown: the
// lowest-risk plannable kind.
const defaultIntentKind = "ensure_fk_indexes"

// changeIntentRequired are each branch's required arguments. On this
// path optimize_query's goal defaults to latency, so only kind is needed.
var changeIntentRequired = map[string][]string{"optimize_query": {"kind"},
	"ensure_fk_indexes": {"kind", "schema"}, "apply_migration": {"kind", "table", "sql"},
	"declare_table_contract": {"kind", "table"},
	"register_consumer":      {"kind", "slot_name", "owner"}}

// intentOwnArguments are names a kind tool takes that an intent never
// carries: the call names the database, and claims are never trusted.
var intentOwnArguments = map[string]bool{"database": true, "database_id": true,
	"caller_claims": true}

const changeIntentDescription = "the change: kind is optimize_query, ensure_fk_indexes, " +
	"apply_migration, declare_table_contract or register_consumer, plus that kind's " +
	"arguments (such as query_id, schema, table or slot_name); see oneOf for each kind"

// changeIntentSchema is the intent property of request_change, built from
// the kind tools' own schemas so the two cannot drift.
func changeIntentSchema(tools []Tool) map[string]any {
	branches := make([]any, 0, len(changeIntentKinds))
	for _, kind := range changeIntentKinds {
		properties := map[string]any{"kind": map[string]any{"const": kind}}
		for name, property := range toolProperties(tools, kind) {
			if !intentOwnArguments[name] {
				properties[name] = property
			}
		}
		var example any
		_ = json.Unmarshal([]byte(changeIntentExamples[kind]), &example)
		branches = append(branches, map[string]any{"title": kind, "type": "object",
			"properties": properties, "required": changeIntentRequired[kind],
			"additionalProperties": false, "examples": []any{example}})
	}
	return map[string]any{"type": "object", "description": changeIntentDescription,
		"properties": map[string]any{"kind": map[string]any{"type": "string",
			"enum": changeIntentKinds}},
		"required": []string{"kind"}, "oneOf": branches}
}

// toolProperties are the properties of the named tool's input schema.
func toolProperties(tools []Tool, name string) map[string]any {
	for _, tool := range tools {
		if tool.Name != name {
			continue
		}
		var schema struct {
			Properties map[string]any `json:"properties"`
		}
		_ = json.Unmarshal(tool.InputSchema, &schema)
		return schema.Properties
	}
	return nil
}

// withRequestChangeSchema gives request_change its typed intent.
func withRequestChangeSchema(tools []Tool, dbID string) []Tool {
	intent, err := json.Marshal(changeIntentSchema(tools))
	if err != nil {
		panic(fmt.Sprintf("request_change intent schema: %v", err))
	}
	for i := range tools {
		if tools[i].Name == "request_change" {
			tools[i].InputSchema = json.RawMessage(`{"type":"object","properties":{` + dbID +
				`,"intent":` + string(intent) + `,"caller_claims":{"type":"object"}},` +
				`"required":["intent"],"additionalProperties":false}`)
		}
	}
	return tools
}

var kindList = strings.Join(changeIntentKinds, ", ")

// validateChangeRequest checks request_change's arguments and its intent
// against the branch of its kind.
func validateChangeRequest(arguments json.RawMessage) *rpcError {
	if failed := strictIntent[struct {
		Intent       jsonObject `json:"intent"`
		CallerClaims jsonObject `json:"caller_claims"`
	}]("request_change takes database, intent and caller_claims")(arguments); failed != nil {
		return failed
	}
	var call struct {
		Intent json.RawMessage `json:"intent"`
	}
	_ = json.Unmarshal(arguments, &call)
	if emptyJSON(bytes.TrimSpace(call.Intent)) {
		return failure(codeInvalidParams, "invalid arguments: intent is required: an "+
			"object with kind (one of "+kindList+") plus that kind's arguments")
	}
	return validateChangeIntent(call.Intent)
}

// validateChangeIntent checks one intent against its kind's branch: the
// kind's own validator, then the branch's required arguments.
func validateChangeIntent(intent json.RawMessage) *rpcError {
	var object map[string]json.RawMessage
	if json.Unmarshal(intent, &object) != nil {
		return failure(codeInvalidParams, "invalid arguments: intent must be an object")
	}
	var kind string
	if raw, ok := object["kind"]; !ok || json.Unmarshal(raw, &kind) != nil || kind == "" {
		return failure(codeInvalidParams, "invalid arguments: change intent kind is "+
			"required; kind is one of "+kindList)
	}
	if _, known := changeIntentExamples[kind]; !known {
		return failure(codeInvalidParams, fmt.Sprintf("invalid arguments: unsupported "+
			"mutation intent %q; kind is one of %s", printableName(kind), kindList))
	}
	for name := range intentOwnArguments {
		if _, ok := object[name]; ok {
			return failure(codeInvalidParams, "invalid arguments: intent never carries "+
				name+"; name it beside intent")
		}
	}
	delete(object, "kind")
	rest, err := json.Marshal(object)
	if err != nil {
		return failure(codeInternal, "internal error")
	}
	if failed := intentValidators[kind](rest); failed != nil {
		return failed
	}
	for _, name := range changeIntentRequired[kind][1:] {
		if _, ok := object[name]; !ok {
			return failure(codeInvalidParams, fmt.Sprintf("invalid arguments: a %s intent "+
				"needs %s", kind, strings.Join(changeIntentRequired[kind][1:], ", ")))
		}
	}
	return nil
}

// intentExample is the next call's example for a refused request_change:
// an intent of the kind it named, or of the default kind.
func intentExample(arguments json.RawMessage) map[string]any {
	kind := intentKind(arguments)
	if _, known := changeIntentExamples[kind]; !known {
		kind = defaultIntentKind
	}
	var intent any
	_ = json.Unmarshal([]byte(changeIntentExamples[kind]), &intent)
	return map[string]any{"intent": intent}
}
