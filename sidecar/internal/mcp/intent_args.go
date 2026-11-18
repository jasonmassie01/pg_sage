package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
)

// Strict intent arguments and typed intent failures (mcp-08). Every
// intent tool's schema says additionalProperties:false; the validators
// below enforce each schema's property set and types before any backend
// sees the call.

// intentValidators check one intent tool's arguments against its schema
// (request_change: validateChangeRequest). An intent tool without a
// validator is refused (fail closed).
var intentValidators = map[string]func(json.RawMessage) *rpcError{
	"get_policy": strictIntent[struct{}]("get_policy takes database"),
	"propose_policy_change": strictIntent[struct {
		Delta        jsonObject `json:"delta"`
		CallerClaims jsonObject `json:"caller_claims"`
	}]("propose_policy_change takes database, delta and caller_claims"),
	"get_ledger": strictIntent[struct {
		Filter jsonObject `json:"filter"`
	}]("get_ledger takes database and filter (an object)"),
	"optimize_query": strictIntent[struct {
		QueryID      int64      `json:"query_id"`
		QueryText    string     `json:"query_text"`
		Goal         string     `json:"goal"`
		Constraints  jsonObject `json:"constraints"`
		CallerClaims jsonObject `json:"caller_claims"`
	}]("optimize_query takes database, query_id (integer), query_text, goal and " +
		"constraints (an object)"),
	"apply_migration": validateMigrationArguments,
	"ensure_fk_indexes": strictIntent[struct {
		Schema       string     `json:"schema"`
		CallerClaims jsonObject `json:"caller_claims"`
	}]("ensure_fk_indexes takes database and schema"),
	"declare_table_contract": strictIntent[tableContractArgs]("declare_table_contract " +
		"takes database, table, append_only, retention {interval, column}, expected_pk " +
		"and exemptions (strings)"),
	"register_consumer": strictIntent[struct {
		SlotName     string     `json:"slot_name"`
		Owner        string     `json:"owner"`
		Identity     string     `json:"consumer_identity"`
		CallerClaims jsonObject `json:"caller_claims"`
	}]("register_consumer takes database, slot_name, owner and consumer_identity"),
	"set_maintenance_policy": strictIntent[struct {
		Scope jsonObject `json:"scope"`
		Patch jsonObject `json:"patch"`
	}]("set_maintenance_policy takes database, scope and patch (objects)"),
	"get_guarantee_status": strictIntent[struct{}]("get_guarantee_status takes database"),
	"get_value":            strictIntent[struct{}]("get_value takes database"),
}

type tableContractArgs struct {
	Table        string         `json:"table"`
	AppendOnly   bool           `json:"append_only"`
	Retention    *retentionArgs `json:"retention"`
	ExpectedPK   string         `json:"expected_pk"`
	Exemptions   []string       `json:"exemptions"`
	CallerClaims jsonObject     `json:"caller_claims"`
}

// retentionArgs types retention's interval and column; its schema does
// not forbid other names, so they are not checked.
type retentionArgs struct{}

func (*retentionArgs) UnmarshalJSON(raw []byte) error {
	if !isJSONObject(raw) {
		return errNotObject
	}
	var fields struct {
		Interval string `json:"interval"`
		Column   string `json:"column"`
	}
	return json.Unmarshal(raw, &fields)
}

var errNotObject = errors.New("value must be a JSON object")

// jsonObject is an argument whose schema type is object; null counts as
// absent.
type jsonObject struct{}

func (*jsonObject) UnmarshalJSON(raw []byte) error {
	if string(bytes.TrimSpace(raw)) == "null" || isJSONObject(raw) {
		return nil
	}
	return errNotObject
}

func isJSONObject(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// strictIntent validates arguments by decoding them strictly into T.
func strictIntent[T any](takes string) func(json.RawMessage) *rpcError {
	return func(arguments json.RawMessage) *rpcError {
		var args T
		if !decodeIntentStrict(arguments, &args) {
			return failure(codeInvalidParams, "invalid arguments: "+takes)
		}
		return nil
	}
}

// decodeIntentStrict decodes arguments into target, rejecting names
// target does not declare. database and database_id are bound by the
// server (bindDatabase writes them back after checking them), so they are
// exempt here.
func decodeIntentStrict(arguments json.RawMessage, target any) bool {
	var object map[string]json.RawMessage
	if len(arguments) > 0 && json.Unmarshal(arguments, &object) != nil {
		return false
	}
	delete(object, "database")
	delete(object, "database_id")
	stripped, err := json.Marshal(object)
	return err == nil && decodeStrict(stripped, target)
}

func validateIntentArguments(name string, arguments json.RawMessage) *rpcError {
	if name == "request_change" { // its intent is checked against its kind's branch
		return validateChangeRequest(arguments)
	}
	validate, ok := intentValidators[name]
	if !ok {
		slog.Error("MCP intent tool has no argument validator", "tool", name)
		return failure(codeInternal, "internal error")
	}
	return validate(arguments)
}

// intentResult wraps an intent backend's result, mapping its typed
// failures: a request to fix (-32602) or a dependency to retry later
// (-32010). Anything else is an internal error whose detail is logged,
// never echoed.
func intentResult(ctx context.Context, tool string, result any, err error) (any,
	*rpcError) {
	switch {
	case err == nil, errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return toolResult(result, err)
	case errors.Is(err, ErrIntentInvalid):
		return nil, failure(codeInvalidParams, "invalid arguments: "+err.Error())
	case errors.Is(err, ErrIntentUnavailable):
		slog.WarnContext(ctx, "MCP intent dependency unavailable", "tool", tool,
			"error", err)
		return nil, failure(codeUnavailable, "unavailable: the target database or a "+
			"pg_sage dependency is unavailable; retry later")
	}
	slog.ErrorContext(ctx, "MCP intent tool failed", "tool", tool, "error", err)
	return nil, failure(codeInternal, "internal error")
}
