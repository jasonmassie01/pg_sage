package config

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// audit-12: docs/security.md names configuration keys an operator copies
// into their YAML. Every retention.* and audit.* key it names must exist,
// and the Audit Trail section must name the retention key each audit
// table really ages out on (rules in internal/retention).

// No concurrent access tests: the tests read a file and walk types.

var securityDocKey = regexp.MustCompile("`((?:retention|audit)\\.[a-z0-9_.]+)`")

// configKeys lists every yaml path of Config: leaves, and the structs and
// lists that hold them (a doc may name audit.siem.sinks or one of its
// fields as audit.siem.sinks.ca_file).
func configKeys(t reflect.Type, prefix string, out map[string]bool) {
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		name := strings.SplitN(sf.Tag.Get("yaml"), ",", 2)[0]
		if !sf.IsExported() || name == "" || name == "-" {
			continue
		}
		key := name
		if prefix != "" {
			key = prefix + "." + name
		}
		out[key] = true
		ft := sf.Type
		for ft.Kind() == reflect.Pointer || ft.Kind() == reflect.Slice {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && ft.PkgPath() == t.PkgPath() {
			configKeys(ft, key, out)
		}
	}
}

func readSecurityDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "security.md"))
	if err != nil {
		t.Fatalf("read docs/security.md: %v", err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

// auditTrailSection is the "## Audit Trail" section of the doc.
func auditTrailSection(t *testing.T, doc string) string {
	t.Helper()
	start := strings.Index(doc, "\n## Audit Trail\n")
	if start < 0 {
		t.Fatal("docs/security.md has no \"## Audit Trail\" section")
	}
	rest := doc[start+1:]
	if end := strings.Index(rest[3:], "\n## "); end >= 0 {
		rest = rest[:end+3]
	}
	return rest
}

func TestSecurityDocKeysExist(t *testing.T) {
	keys := map[string]bool{}
	configKeys(reflect.TypeOf(Config{}), "", keys)
	if !keys["retention.auth_audit_days"] || !keys["audit.siem.sinks.ca_file"] {
		t.Fatal("fixture: the key walk misses known keys; the check would be vacuous")
	}
	var missing []string
	seen := 0
	for _, m := range securityDocKey.FindAllStringSubmatch(readSecurityDoc(t), -1) {
		seen++
		if !keys[strings.TrimSuffix(m[1], ".")] {
			missing = append(missing, m[1])
		}
	}
	if seen < 5 {
		t.Fatalf("docs/security.md names only %d retention/audit keys; the regexp is "+
			"broken or the doc lost its audit sections", seen)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("docs/security.md names keys that do not exist: %v", missing)
	}
}

// auditRetention is the key (or "kept") of each audit table, as
// internal/retention/rules.go and exemptions.go apply them.
var auditRetention = map[string]string{
	"action_log":           "retention.actions_days",
	"auth_audit":           "retention.auth_audit_days",
	"guard_pgaudit_events": "retention.actions_days",
	"guard_query_audit":    "agents.query.audit_retention_days",
}

func TestSecurityDocNamesEachAuditTablesRetentionKey(t *testing.T) {
	section := auditTrailSection(t, readSecurityDoc(t))
	if strings.Contains(section, "Both tables are subject to retention policies") {
		t.Error("the Audit Trail section still says both tables use " +
			"retention.actions_days; auth_audit uses retention.auth_audit_days")
	}
	for table, key := range auditRetention {
		row := regexp.MustCompile("\\| `(sage\\.)?" + regexp.QuoteMeta(table) +
			"` \\|[^\n]*`" + regexp.QuoteMeta(key) + "`")
		if !row.MatchString(section) {
			t.Errorf("the Audit Trail retention table has no row for %s naming %s",
				table, key)
		}
	}
	for _, kept := range []string{"config_audit", "fact_events"} {
		row := regexp.MustCompile("\\| `(sage\\.)?" + kept + "` \\|[^\n]*[Kk]ept")
		if !row.MatchString(section) {
			t.Errorf("the Audit Trail retention table does not say %s is kept", kept)
		}
	}
}

// The Audit Trail section documents what an operator monitors: the
// metrics, the status route and the threat model.
func TestSecurityDocAuditTrailCoversMonitoringAndThreatModel(t *testing.T) {
	section := auditTrailSection(t, readSecurityDoc(t))
	for _, want := range []string{"pg_sage_audit_chain_ok", "pg_sage_siem_lag_links",
		"pg_sage_audit_chain_verify_errors_total", "GET /api/v1/audit/status",
		"### Threat model", "fleet leader"} {
		if !strings.Contains(section, want) {
			t.Errorf("the Audit Trail section does not mention %s", want)
		}
	}
}
