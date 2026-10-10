package siem

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

func link(chain string, seq int64, op string) auditchain.Link {
	return auditchain.Link{Seq: seq, RowID: 7, Op: op, V: 1, SealedHash: "s", State: "",
		PrevHash: "p", Hash: "h", At: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC),
		DBUser: "sage", App: "pg_sage"}
}

func num(t *testing.T, e Event, key string) int {
	t.Helper()
	v, ok := e[key].(int)
	if !ok {
		t.Fatalf("%s = %#v, want an int", key, e[key])
	}
	return v
}

// Every event carries the OCSF base attributes, and type_uid is
// class_uid * 100 + activity_id.
func TestEveryEventHasOCSFBaseAttributes(t *testing.T) {
	records := []Record{
		{Source: "orders", Chain: "action_log", Link: link("action_log", 1, "I"),
			Row: map[string]any{"sql_executed": "CREATE INDEX CONCURRENTLY i ON t (a)",
				"outcome": "success", "action_type": "create_index"}},
		{Source: "orders", Chain: "action_log", Link: link("action_log", 2, "U"),
			Row: map[string]any{"outcome": "rolled_back"}},
		{Source: "control", Chain: "auth_audit", Link: link("auth_audit", 1, "I"),
			Row: map[string]any{"event": "login_failed", "target_user_id": 3,
				"source_ip": "10.0.0.9"}},
		{Source: "control", Chain: "config_audit", Link: link("config_audit", 1, "I"),
			Row: map[string]any{"key": "trust.level", "old_value": "observation",
				"new_value": "advisory"}},
		{Source: "orders", Chain: "pgaudit_events", Link: link("pgaudit_events", 1, "I"),
			Row: map[string]any{"class": "DDL", "command": "CREATE INDEX"}},
		{Source: "orders", Chain: "guard_query_audit", Link: link("guard_query_audit", 1, "I"),
			Row: map[string]any{"verdict": "denied"}},
		{Source: "orders", Chain: "action_log", Link: link("action_log", 3, "D")},
		{Source: "orders", Chain: "action_log", Link: link("action_log", 4, "T")},
	}
	for _, r := range records {
		e := Map(r)
		class, activity := num(t, e, "class_uid"), num(t, e, "activity_id")
		if num(t, e, "type_uid") != class*100+activity || num(t, e, "category_uid") !=
			class/1000 || num(t, e, "severity_id") < 1 {
			t.Fatalf("%s %s: base attributes = %v", r.Chain, r.Link.Op, e)
		}
		if e["time"] != r.Link.At.UnixMilli() {
			t.Fatalf("%s: time = %v", r.Chain, e["time"])
		}
		md, _ := e["metadata"].(map[string]any)
		product, _ := md["product"].(map[string]any)
		if md["version"] != OCSFVersion || product["name"] != "pg_sage" ||
			md["uid"] == "" || md["log_name"] != r.Chain {
			t.Fatalf("%s: metadata = %v", r.Chain, md)
		}
		ext := e["unmapped"].(map[string]any)["pg_sage"].(map[string]any)
		if ext["hash"] != "h" || ext["prev_hash"] != "p" || ext["seq"] != r.Link.Seq ||
			ext["op"] != r.Link.Op {
			t.Fatalf("%s: chain proof missing: %v", r.Chain, ext)
		}
		if _, err := json.Marshal(e); err != nil {
			t.Fatalf("%s: not JSON: %v", r.Chain, err)
		}
	}
}

// Classes and activities follow the event: actions are datastore
// activity, sign-ins authentication, user changes account changes, config
// changes entity management.
func TestClassMapping(t *testing.T) {
	cases := []struct {
		chain, op string
		row       map[string]any
		class     int
		activity  int
		status    int
	}{
		{"action_log", "I", map[string]any{"sql_executed": "CREATE INDEX x ON t (a)",
			"outcome": "success"}, ClassDatastoreActivity, 6, 1},
		{"action_log", "I", map[string]any{"sql_executed": "DROP INDEX x",
			"outcome": "failed"}, ClassDatastoreActivity, 7, 2},
		{"action_log", "I", map[string]any{"sql_executed": "VACUUM (ANALYZE) t",
			"outcome": "pending"}, ClassDatastoreActivity, 2, 0},
		{"action_log", "U", map[string]any{"outcome": "success"}, ClassDatastoreActivity,
			99, 1},
		{"auth_audit", "I", map[string]any{"event": "login_succeeded"},
			ClassAuthentication, 1, 1},
		{"auth_audit", "I", map[string]any{"event": "break_glass_login_failed"},
			ClassAuthentication, 1, 2},
		{"auth_audit", "I", map[string]any{"event": "user_created"}, ClassAccountChange,
			1, 1},
		{"auth_audit", "I", map[string]any{"event": "user_deleted"}, ClassAccountChange,
			6, 1},
		{"auth_audit", "I", map[string]any{"event": "user_role_changed"},
			ClassAccountChange, 99, 1},
		{"config_audit", "I", map[string]any{"key": "trust.level"},
			ClassEntityManagement, 3, 1},
		{"guard_query_audit", "I", map[string]any{"verdict": "allowed"},
			ClassDatastoreActivity, 4, 1},
		{"pgaudit_events", "I", map[string]any{"class": "READ"}, ClassDatastoreActivity,
			1, 1},
		{"pgaudit_events", "I", map[string]any{"class": "WRITE"}, ClassDatastoreActivity,
			5, 1},
	}
	for i, c := range cases {
		e := Map(Record{Source: "db", Chain: c.chain, Link: link(c.chain, 1, c.op),
			Row: c.row})
		if num(t, e, "class_uid") != c.class || num(t, e, "activity_id") != c.activity ||
			num(t, e, "status_id") != c.status {
			t.Fatalf("case %d (%s %v): class %v activity %v status %v", i, c.chain, c.row,
				e["class_uid"], e["activity_id"], e["status_id"])
		}
	}
}

// Audit-trail removals are flagged: a truncate is high severity, a delete
// outside pg_sage's own retention medium.
func TestTrailRemovalSeverity(t *testing.T) {
	trunc := Map(Record{Source: "db", Chain: "action_log", Link: link("action_log", 9, "T")})
	if num(t, trunc, "severity_id") != SeverityHigh {
		t.Fatalf("truncate severity = %v", trunc["severity_id"])
	}
	retention := Map(Record{Source: "db", Chain: "auth_audit",
		Link: link("auth_audit", 9, "D")})
	if num(t, retention, "severity_id") != SeverityInformational {
		t.Fatalf("pg_sage retention delete severity = %v", retention["severity_id"])
	}
	l := link("auth_audit", 10, "D")
	l.App = "psql"
	manual := Map(Record{Source: "db", Chain: "auth_audit", Link: l})
	if num(t, manual, "severity_id") != SeverityMedium {
		t.Fatalf("manual delete severity = %v", manual["severity_id"])
	}
}

// Secret configuration values never leave pg_sage; ordinary ones do.
func TestConfigSecretsAreRedacted(t *testing.T) {
	secret := Map(Record{Source: "control", Chain: "config_audit",
		Link: link("config_audit", 1, "I"), Row: map[string]any{"key": "llm.api_key",
			"old_value": "sk-old", "new_value": "sk-new"}})
	body, _ := json.Marshal(secret)
	if strings.Contains(string(body), "sk-old") || strings.Contains(string(body), "sk-new") {
		t.Fatalf("secret leaked: %s", body)
	}
	unknown := Map(Record{Source: "control", Chain: "config_audit",
		Link: link("config_audit", 2, "I"), Row: map[string]any{"key": "made.up.key",
			"new_value": "maybe-secret"}})
	body, _ = json.Marshal(unknown)
	if strings.Contains(string(body), "maybe-secret") {
		t.Fatalf("an unknown key's value was exported: %s", body)
	}
	plain := Map(Record{Source: "control", Chain: "config_audit",
		Link: link("config_audit", 3, "I"), Row: map[string]any{"key": "trust.level",
			"old_value": "observation", "new_value": "advisory"}})
	body, _ = json.Marshal(plain)
	if !strings.Contains(string(body), "advisory") {
		t.Fatalf("an ordinary value was redacted: %s", body)
	}
}

// Oversized fields are truncated so one event fits a syslog datagram.
func TestOversizedFieldsAreTruncated(t *testing.T) {
	e := Map(Record{Source: "db", Chain: "action_log", Link: link("action_log", 1, "I"),
		Row: map[string]any{"sql_executed": strings.Repeat("x", 100000),
			"before_state": map[string]any{"big": strings.Repeat("y", 100000)}}})
	body, _ := json.Marshal(e)
	if len(body) > MaxEventBytes {
		t.Fatalf("event is %d bytes, cap %d", len(body), MaxEventBytes)
	}
	if !strings.Contains(string(body), "truncated") {
		t.Fatalf("truncation not marked")
	}
}

// A record with no row (deleted, or a nil map) still maps.
func TestNilRowMaps(t *testing.T) {
	e := Map(Record{Source: "db", Chain: "action_log", Link: link("action_log", 1, "I")})
	if num(t, e, "class_uid") != ClassDatastoreActivity {
		t.Fatalf("nil row event = %v", e)
	}
	unknown := Map(Record{Source: "db", Chain: "some_future_chain",
		Link: link("x", 1, "I")})
	if num(t, unknown, "class_uid") != ClassAPIActivity || num(t, unknown,
		"activity_id") != 99 {
		t.Fatalf("unknown chain event = %v", unknown)
	}
}
