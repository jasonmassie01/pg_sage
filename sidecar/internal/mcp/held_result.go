package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Held results (mcp-01a). A successful call whose work now waits on a
// person says where the person decides (approval_url), what the agent can
// poll meanwhile (status: {tool, arguments}) and the item's handle,
// "<kind>:<id>". Where an inbox item exists the handle is its inbox kind
// and id; a change the policy check held back is "change:<evidence_id>"
// and a proposed plan "plan:<id>". With a handle, status is
// get_request_status {handle, database} and wait is wait_for_decision with
// the same arguments (mcp-01b); without one (no id came back), status
// stays the list the item shows up in. Polling and waiting are read-only:
// no field here approves anything.

// heldItem is what one held result adds.
type heldItem struct {
	approvalURL string
	statusTool  string
	statusArgs  map[string]any
	handle      string
}

// heldShapers read a tool's result; false when nothing waits on a person.
var heldShapers = map[string]func(content map[string]any, database string) (heldItem, bool){
	"request_change": changeHeld, "optimize_query": changeHeld,
	"ensure_fk_indexes": changeHeld, "propose_policy_change": policyHeld,
	"set_maintenance_policy": policyHeld, "specialist_request_remediation": remediationHeld,
	toolRequestCapability: capabilityHeld, "propose_plan": planHeld,
}

// withHeldFields adds a held result's fields to the tools/call result out.
func withHeldFields(tool string, arguments json.RawMessage,
	out map[string]any) map[string]any {
	shaper, ok := heldShapers[tool]
	if !ok {
		return out
	}
	content, ok := asObject(out["structuredContent"])
	if !ok {
		return out
	}
	item, held := shaper(content, boundDatabase(arguments))
	if !held {
		return out
	}
	if _, kept := content["approval_url"]; !kept && item.approvalURL != "" {
		content["approval_url"] = item.approvalURL
	}
	status := map[string]any{"tool": item.statusTool, "arguments": item.statusArgs}
	if item.handle != "" {
		content["handle"] = item.handle
		args := databaseArgs(boundDatabase(arguments))
		args["handle"] = item.handle
		status = map[string]any{"tool": toolGetRequestStatus, "arguments": args}
		content["wait"] = map[string]any{"tool": toolWaitForDecision, "arguments": args}
	}
	// A tool's own status field (propose_plan's plan status) is kept: the
	// held fields are additive. handle and wait still name the item.
	if _, own := content["status"]; !own {
		content["status"] = status
	}
	return toolSuccess(content)
}

// asObject is a result as a JSON object, numbers kept exact.
func asObject(result any) (map[string]any, bool) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if decoder.Decode(&object) != nil || object == nil {
		return nil, false
	}
	return object, true
}

// boundDatabase is the database the server bound the call to, if any.
func boundDatabase(arguments json.RawMessage) string {
	var call struct {
		Database string `json:"database"`
	}
	_ = json.Unmarshal(arguments, &call)
	return call.Database
}

// changeHeld: a change the policy check queued for approval or parked is
// a withheld decision in the ledger. The intent path creates no approval
// item, so the agent polls get_ledger, never the inbox's change
// approvals, and a person sees the decision on the Actions decisions tab.
func changeHeld(content map[string]any, database string) (heldItem, bool) {
	switch text(content["decision"]) {
	case "queued", "parked":
	default:
		return heldItem{}, false
	}
	return heldItem{handle: prefixed("change", text(content["evidence_id"])),
		approvalURL: page("actions", "tab", "decisions", "database", database),
		statusTool:  "get_ledger", statusArgs: databaseArgs(database)}, true
}

// policyHeld: a policy proposal waits for an admin's ratification.
func policyHeld(content map[string]any, database string) (heldItem, bool) {
	id := text(content["proposal_id"])
	if id == "" || id == "0" {
		return heldItem{}, false
	}
	return heldItem{approvalURL: page("agents", "tab", "policy", "proposal", id,
		"database", database), statusTool: toolGetInbox,
		statusArgs: inboxStatus(database, "policy_exception"),
		handle:     "policy_exception:" + id}, true
}

// remediationHeld: a requested remediation queued for a person's approval.
func remediationHeld(content map[string]any, database string) (heldItem, bool) {
	switch text(content["verdict"]) {
	case "queued_for_approval", "already_requested":
	default:
		return heldItem{}, false
	}
	item := heldItem{statusTool: toolGetInbox,
		statusArgs:  inboxStatus(database, "change_approval"),
		approvalURL: page("actions", "tab", "pending", "database", database)}
	if queue := text(content["approval_queue_id"]); queue != "" && queue != "0" {
		item.approvalURL = page("actions", "tab", "pending", "queue", queue,
			"database", database)
		item.handle = "change_approval:" + queue
	}
	return item, true
}

// capabilityHeld: a capability request waiting for an operator; its own
// approval_url is kept.
func capabilityHeld(content map[string]any, database string) (heldItem, bool) {
	if text(content["verdict"]) != VerdictQueueApproval {
		return heldItem{}, false
	}
	id := text(content["request_id"])
	item := heldItem{statusTool: toolGetInbox,
		statusArgs: inboxStatus(database, "agent_request")}
	if id != "" && id != "0" {
		item.handle = "agent_request:" + id
	}
	return item, true
}

// planHeld: a proposed plan waits for a person's approval.
func planHeld(content map[string]any, database string) (heldItem, bool) {
	id := text(content["plan_id"])
	if text(content["status"]) != "proposed" || id == "" || id == "0" {
		return heldItem{}, false
	}
	return heldItem{handle: "plan:" + id,
		approvalURL: page("actions", "tab", "plans", "plan", id, "database", database),
		statusTool:  toolGetInbox, statusArgs: inboxStatus(database, "plan_approval")}, true
}

// inboxStatus are get_inbox's arguments for one kind on one database.
func inboxStatus(database, kind string) map[string]any {
	args := databaseArgs(database)
	args["kinds"] = []string{kind}
	return args
}

func databaseArgs(database string) map[string]any {
	if database == "" {
		return map[string]any{}
	}
	return map[string]any{"database": database}
}

// page is a dashboard hash route, the form inbox item hrefs use; empty
// values are left out.
func page(name string, pairs ...string) string {
	var query []string
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" {
			query = append(query, pairs[i]+"="+url.QueryEscape(pairs[i+1]))
		}
	}
	if len(query) == 0 {
		return "#/" + name
	}
	return "#/" + name + "?" + strings.Join(query, "&")
}

func prefixed(kind, id string) string {
	if id == "" {
		return ""
	}
	return kind + ":" + id
}

// text is a JSON scalar as a string ("" for null or absent).
func text(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}
