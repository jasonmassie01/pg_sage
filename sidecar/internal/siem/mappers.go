package siem

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/auditchain"
	"github.com/pg-sage/sidecar/internal/config"
)

// actionMapping: pg_sage executed (I) or re-judged (U) a change.
func actionMapping(r Record) mapping {
	outcome := stateOutcome(r.Link.State)
	if outcome == "" {
		outcome = str(r.Row, "outcome")
	}
	sql := str(r.Row, "sql_executed")
	m := mapping{class: ClassDatastoreActivity, status: outcomeStatus(outcome),
		severity: SeverityInformational,
		extra: map[string]any{"database": databaseObject(r.Source),
			"query_info": map[string]any{"query_string": capString(sql)}}}
	if r.Link.Op == "U" {
		m.activity, m.activityName = 99, "Outcome Changed"
		m.message = fmt.Sprintf("pg_sage action %d is now %s", r.Link.RowID, outcome)
		return m
	}
	m.activity, m.activityName = sqlActivity(sql)
	m.message = fmt.Sprintf("pg_sage action %d (%s): %s", r.Link.RowID,
		str(r.Row, "action_type"), outcome)
	if m.status == statusFailure {
		m.severity = SeverityLow
	}
	return m
}

// stateOutcome reads the outcome (the first state column) a link recorded.
func stateOutcome(state string) string {
	var cols []any
	if state == "" || json.Unmarshal([]byte(state), &cols) != nil || len(cols) == 0 {
		return ""
	}
	s, _ := cols[0].(string)
	return s
}

func outcomeStatus(outcome string) int {
	switch outcome {
	case "success", "rolled_back", "verified":
		return statusSuccess
	case "failed", "rollback_failed":
		return statusFailure
	}
	return statusUnknown
}

// sqlActivity maps a statement's verb to a Datastore Activity activity.
func sqlActivity(sql string) (int, string) {
	fields := strings.Fields(strings.ToUpper(sql))
	verb := ""
	if len(fields) > 0 {
		verb = fields[0]
	}
	switch verb {
	case "CREATE":
		return 6, "Create"
	case "DROP", "DELETE", "TRUNCATE":
		return 7, "Delete"
	case "ALTER", "VACUUM", "ANALYZE", "REINDEX", "CLUSTER", "UPDATE", "SET", "RESET",
		"COMMENT", "GRANT", "REVOKE":
		return 2, "Update"
	case "SELECT":
		return 4, "Query"
	case "INSERT", "COPY":
		return 5, "Write"
	}
	return 99, "Other"
}

// authMapping: sign-ins are Authentication; user administration and SSO
// account links are Account Change.
func authMapping(r Record) mapping {
	ev := str(r.Row, "event")
	m := mapping{class: ClassAccountChange, status: statusSuccess,
		severity: SeverityInformational, message: "pg_sage " + ev,
		extra: map[string]any{"user": map[string]any{"uid": str(r.Row, "target_user_id")},
			"src_endpoint": map[string]any{"ip": str(r.Row, "source_ip")}}}
	switch ev {
	case "login_succeeded", "login_failed", "break_glass_login", "break_glass_login_failed":
		m.class, m.activity, m.activityName = ClassAuthentication, 1, "Logon"
		if strings.HasSuffix(ev, "_failed") {
			m.status, m.severity = statusFailure, SeverityLow
		}
		if strings.HasPrefix(ev, "break_glass") {
			m.severity = SeverityMedium
		}
	case "user_created":
		m.activity, m.activityName = 1, "Create"
	case "user_deleted":
		m.activity, m.activityName = 6, "Delete"
	case "user_role_changed":
		m.activity, m.activityName = 99, "Role Change"
	default:
		m.activity, m.activityName = 99, "Other"
	}
	return m
}

// configMapping: a configuration change, its values redacted unless the
// key is a known, non-secret one.
func configMapping(r Record) mapping {
	key := str(r.Row, "key")
	entity := map[string]any{"name": key, "type": "pg_sage configuration"}
	if exportableConfigKey(key) {
		entity["data"] = map[string]any{"old_value": str(r.Row, "old_value"),
			"new_value": str(r.Row, "new_value")}
	}
	return mapping{class: ClassEntityManagement, activity: 3, activityName: "Update",
		status: statusSuccess, severity: SeverityInformational,
		message: "pg_sage configuration changed: " + key,
		extra:   map[string]any{"entity": entity}}
}

// redactRow removes configuration values that may be credentials from the
// copy of the row an event carries.
func redactRow(r Record) map[string]any {
	if r.Chain != auditchain.ConfigAudit.Chain || r.Row == nil ||
		exportableConfigKey(str(r.Row, "key")) {
		return r.Row
	}
	out := make(map[string]any, len(r.Row))
	for k, v := range r.Row {
		out[k] = v
	}
	for _, k := range []string{"old_value", "new_value"} {
		if _, ok := out[k]; ok {
			out[k] = "(redacted)"
		}
	}
	return out
}

// exportableConfigKey: only keys the configuration describes as plain
// values (never a credential, endpoint secret or unknown key).
func exportableConfigKey(key string) bool {
	doc, ok := config.DescribeField(nil, key)
	return ok && !doc.Secret && doc.HasValue
}

func queryAuditMapping(r Record) mapping {
	status := statusSuccess
	if v := str(r.Row, "verdict"); v != "allowed" && v != "allow" && v != "" {
		status = statusFailure
	}
	return mapping{class: ClassDatastoreActivity, activity: 4, activityName: "Query",
		status: status, severity: SeverityInformational,
		message: "brokered agent query by " + str(r.Row, "principal_id"),
		extra:   map[string]any{"database": databaseObject(r.Source)}}
}

// pgauditMapping: the database's own record of a statement pg_sage or an
// agent ran.
func pgauditMapping(r Record) mapping {
	m := mapping{class: ClassDatastoreActivity, status: statusSuccess,
		severity: SeverityInformational,
		message:  "pgaudit " + str(r.Row, "class") + " " + str(r.Row, "command"),
		extra: map[string]any{"database": databaseObject(r.Source),
			"query_info": map[string]any{"query_string": capString(str(r.Row,
				"statement"))}}}
	switch str(r.Row, "class") {
	case "READ":
		m.activity, m.activityName = 1, "Read"
	case "WRITE":
		m.activity, m.activityName = 5, "Write"
	case "DDL", "ROLE":
		m.activity, m.activityName = sqlActivity(str(r.Row, "command"))
	default:
		m.activity, m.activityName = 99, "Other"
	}
	return m
}

const (
	maxStringBytes = 4096
	maxObjectBytes = 8192
)

func capString(s string) string {
	if len(s) <= maxStringBytes {
		return s
	}
	return s[:maxStringBytes] + fmt.Sprintf("... (truncated, %d bytes)", len(s))
}

// sanitizeRow bounds every value of a row: long strings are cut, large
// objects replaced by their size.
func sanitizeRow(row map[string]any) map[string]any {
	if row == nil {
		return nil
	}
	out := make(map[string]any, len(row))
	for k, v := range row {
		switch val := v.(type) {
		case string:
			out[k] = capString(val)
		case map[string]any, []any:
			body, _ := json.Marshal(val)
			if len(body) > maxObjectBytes {
				out[k] = fmt.Sprintf("(truncated, %d bytes)", len(body))
				continue
			}
			out[k] = val
		default:
			out[k] = val
		}
	}
	return out
}

// fit drops the row copy, then caps the query, when the event is still too
// large to send as one datagram.
func fit(e Event) {
	if body, _ := json.Marshal(e); len(body) <= MaxEventBytes {
		return
	}
	ext := e["unmapped"].(map[string]any)["pg_sage"].(map[string]any)
	ext["row"] = "(truncated: row too large)"
	if body, _ := json.Marshal(e); len(body) <= MaxEventBytes {
		return
	}
	if qi, ok := e["query_info"].(map[string]any); ok {
		qi["query_string"] = "(truncated)"
	}
}
