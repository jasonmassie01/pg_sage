package notify

import (
	"context"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 1. RegisterSender — verify registered senders are stored and usable
// ---------------------------------------------------------------------------

func TestRegisterSender_StoresAndRetrieves(t *testing.T) {
	d := NewDispatcher(nil, func(string, string, ...any) {})

	s := newMockSender("slack")
	d.RegisterSender(s)

	// Verify the sender is stored under its type key.
	got, ok := d.senders["slack"]
	if !ok {
		t.Fatal("expected sender to be registered under 'slack'")
	}
	if got.Type() != "slack" {
		t.Errorf("sender.Type() = %q, want 'slack'", got.Type())
	}
}

func TestRegisterSender_OverwritesPrevious(t *testing.T) {
	d := NewDispatcher(nil, func(string, string, ...any) {})

	s1 := newMockSender("email")
	s2 := newMockSender("email")
	d.RegisterSender(s1)
	d.RegisterSender(s2)

	// Second registration should overwrite first.
	if len(d.senders) != 1 {
		t.Errorf("expected 1 sender, got %d", len(d.senders))
	}
}

func TestRegisterSender_MultipleDifferentTypes(t *testing.T) {
	d := NewDispatcher(nil, func(string, string, ...any) {})

	d.RegisterSender(newMockSender("slack"))
	d.RegisterSender(newMockSender("email"))

	if len(d.senders) != 2 {
		t.Errorf("expected 2 senders, got %d", len(d.senders))
	}

	for _, typ := range []string{"slack", "email"} {
		if _, ok := d.senders[typ]; !ok {
			t.Errorf("expected sender registered for type %q", typ)
		}
	}
}

// SendDirect bypasses DB rule loading and tests the sender path directly.
func TestRegisterSender_UsedViaSendDirect(t *testing.T) {
	d := NewDispatcher(nil, func(string, string, ...any) {})

	s := newMockSender("slack")
	d.RegisterSender(s)

	ch := Channel{
		ID:      1,
		Name:    "test",
		Type:    "slack",
		Config:  map[string]string{"webhook_url": "http://example.com"},
		Enabled: true,
	}
	evt := Event{
		Type:     "action_executed",
		Severity: "info",
		Subject:  "Test",
		Body:     "Body",
	}

	// SendDirect will call sender.Send then logDelivery.
	// logDelivery will fail (nil pool), but the sender should still be invoked.
	_ = d.SendDirect(context.Background(), ch, evt)

	if s.callCount() != 1 {
		t.Errorf("expected sender to be called once, got %d", s.callCount())
	}
}

// ---------------------------------------------------------------------------
// 2. Event factory completeness
// ---------------------------------------------------------------------------

func TestEventFactory_ActionExecutedEvent(t *testing.T) {
	evt := ActionExecutedEvent("Create idx_orders", "CREATE INDEX idx_orders ON orders (id)", "prod")

	if evt.Type != "action_executed" {
		t.Errorf("Type = %q, want 'action_executed'", evt.Type)
	}
	if evt.Severity != "info" {
		t.Errorf("Severity = %q, want 'info'", evt.Severity)
	}
	if evt.Subject == "" {
		t.Error("Subject must not be empty")
	}
	if !strings.Contains(evt.Subject, "Create idx_orders") {
		t.Errorf("Subject %q should contain the title", evt.Subject)
	}
	if evt.Body == "" {
		t.Error("Body must not be empty")
	}
	if !strings.Contains(evt.Body, "prod") {
		t.Errorf("Body %q should contain database name", evt.Body)
	}
	if !strings.Contains(evt.Body, "CREATE INDEX") {
		t.Errorf("Body %q should contain the SQL", evt.Body)
	}

	// Data map keys.
	wantKeys := []string{"title", "sql", "database"}
	for _, k := range wantKeys {
		if _, ok := evt.Data[k]; !ok {
			t.Errorf("Data missing key %q", k)
		}
	}
	if evt.Data["title"] != "Create idx_orders" {
		t.Errorf("Data[title] = %v, want 'Create idx_orders'", evt.Data["title"])
	}
	if evt.Data["sql"] != "CREATE INDEX idx_orders ON orders (id)" {
		t.Errorf("Data[sql] = %v, want the full SQL", evt.Data["sql"])
	}
	if evt.Data["database"] != "prod" {
		t.Errorf("Data[database] = %v, want 'prod'", evt.Data["database"])
	}
}

func TestEventFactory_ActionFailedEvent(t *testing.T) {
	evt := ActionFailedEvent("Drop idx_old", "DROP INDEX idx_old", "staging", "lock timeout")

	if evt.Type != "action_failed" {
		t.Errorf("Type = %q, want 'action_failed'", evt.Type)
	}
	if evt.Severity != "warning" {
		t.Errorf("Severity = %q, want 'warning'", evt.Severity)
	}
	if evt.Subject == "" {
		t.Error("Subject must not be empty")
	}
	if !strings.Contains(evt.Subject, "Drop idx_old") {
		t.Errorf("Subject %q should contain the title", evt.Subject)
	}
	if evt.Body == "" {
		t.Error("Body must not be empty")
	}
	if !strings.Contains(evt.Body, "staging") {
		t.Errorf("Body %q should contain database name", evt.Body)
	}
	if !strings.Contains(evt.Body, "lock timeout") {
		t.Errorf("Body %q should contain the error message", evt.Body)
	}

	wantKeys := []string{"title", "sql", "database", "error"}
	for _, k := range wantKeys {
		if _, ok := evt.Data[k]; !ok {
			t.Errorf("Data missing key %q", k)
		}
	}
	if evt.Data["error"] != "lock timeout" {
		t.Errorf("Data[error] = %v, want 'lock timeout'", evt.Data["error"])
	}
}

func TestEventFactory_ApprovalNeededEvent(t *testing.T) {
	evt := ApprovalNeededEvent("Reindex orders", "REINDEX TABLE orders", "prod", "moderate")

	if evt.Type != "approval_needed" {
		t.Errorf("Type = %q, want 'approval_needed'", evt.Type)
	}
	if evt.Severity != "warning" {
		t.Errorf("Severity = %q, want 'warning'", evt.Severity)
	}
	if evt.Subject == "" {
		t.Error("Subject must not be empty")
	}
	if !strings.Contains(evt.Subject, "Reindex orders") {
		t.Errorf("Subject %q should contain the title", evt.Subject)
	}
	if evt.Body == "" {
		t.Error("Body must not be empty")
	}
	if !strings.Contains(evt.Body, "moderate") {
		t.Errorf("Body %q should contain the risk level", evt.Body)
	}

	wantKeys := []string{"title", "sql", "database", "risk"}
	for _, k := range wantKeys {
		if _, ok := evt.Data[k]; !ok {
			t.Errorf("Data missing key %q", k)
		}
	}
	if evt.Data["risk"] != "moderate" {
		t.Errorf("Data[risk] = %v, want 'moderate'", evt.Data["risk"])
	}
}

func TestEventFactory_FindingCriticalEvent(t *testing.T) {
	evt := FindingCriticalEvent("Sequence exhaustion", "orders_id_seq at 95%", "prod")

	if evt.Type != "finding_critical" {
		t.Errorf("Type = %q, want 'finding_critical'", evt.Type)
	}
	if evt.Severity != "critical" {
		t.Errorf("Severity = %q, want 'critical'", evt.Severity)
	}
	if evt.Subject == "" {
		t.Error("Subject must not be empty")
	}
	if !strings.Contains(evt.Subject, "Sequence exhaustion") {
		t.Errorf("Subject %q should contain the title", evt.Subject)
	}
	if evt.Body == "" {
		t.Error("Body must not be empty")
	}
	if !strings.Contains(evt.Body, "orders_id_seq at 95%") {
		t.Errorf("Body %q should contain the detail", evt.Body)
	}

	wantKeys := []string{"title", "detail", "database"}
	for _, k := range wantKeys {
		if _, ok := evt.Data[k]; !ok {
			t.Errorf("Data missing key %q", k)
		}
	}
	if evt.Data["detail"] != "orders_id_seq at 95%" {
		t.Errorf("Data[detail] = %v, want 'orders_id_seq at 95%%'", evt.Data["detail"])
	}
}

// ---------------------------------------------------------------------------
// 3. SeverityMeetsMin edge cases
// ---------------------------------------------------------------------------

func TestSeverityMeetsMin_EdgeCases(t *testing.T) {
	tests := []struct {
		name string
		sev  string
		min  string
		want bool
	}{
		{
			name: "empty min severity defaults to rank 0 (info)",
			sev:  "info",
			min:  "",
			want: true, // ValidSeverities[""] == 0, ValidSeverities["info"] == 0
		},
		{
			name: "empty min severity with warning",
			sev:  "warning",
			min:  "",
			want: true, // 1 >= 0
		},
		{
			name: "empty min severity with critical",
			sev:  "critical",
			min:  "",
			want: true, // 2 >= 0
		},
		{
			name: "unknown severity string as event severity",
			sev:  "bogus",
			min:  "info",
			want: true, // ValidSeverities["bogus"] == 0 >= ValidSeverities["info"] == 0
		},
		{
			name: "unknown severity as min with info event",
			sev:  "info",
			min:  "bogus",
			want: true, // 0 >= 0
		},
		{
			name: "unknown severity as min with critical event",
			sev:  "critical",
			min:  "bogus",
			want: true, // 2 >= 0
		},
		{
			name: "both unknown severities",
			sev:  "foo",
			min:  "bar",
			want: true, // 0 >= 0
		},
		{
			name: "empty event severity with critical min",
			sev:  "",
			min:  "critical",
			want: false, // 0 >= 2 is false
		},
		{
			name: "empty event severity with warning min",
			sev:  "",
			min:  "warning",
			want: false, // 0 >= 1 is false
		},
		{
			name: "empty both",
			sev:  "",
			min:  "",
			want: true, // 0 >= 0
		},
		{
			name: "same severity info",
			sev:  "info",
			min:  "info",
			want: true,
		},
		{
			name: "same severity warning",
			sev:  "warning",
			min:  "warning",
			want: true,
		},
		{
			name: "same severity critical",
			sev:  "critical",
			min:  "critical",
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SeverityMeetsMin(tc.sev, tc.min)
			if got != tc.want {
				t.Errorf("SeverityMeetsMin(%q, %q) = %v, want %v",
					tc.sev, tc.min, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 4. ValidEventTypes map completeness
// ---------------------------------------------------------------------------

func TestValidEventTypes_Completeness(t *testing.T) {
	expectedTypes := map[string]bool{
		"action_executed":  true,
		"action_failed":    true,
		"approval_needed":  true,
		"finding_critical": true,
	}

	// Verify exact count.
	if len(ValidEventTypes) != 4 {
		t.Errorf("ValidEventTypes has %d entries, want 4", len(ValidEventTypes))
	}

	// Verify all expected types are present and true.
	for typ, wantVal := range expectedTypes {
		gotVal, exists := ValidEventTypes[typ]
		if !exists {
			t.Errorf("ValidEventTypes missing %q", typ)
			continue
		}
		if gotVal != wantVal {
			t.Errorf("ValidEventTypes[%q] = %v, want %v", typ, gotVal, wantVal)
		}
	}

	// Verify no extra types exist.
	for typ := range ValidEventTypes {
		if _, expected := expectedTypes[typ]; !expected {
			t.Errorf("ValidEventTypes has unexpected type %q", typ)
		}
	}

	// Verify invalid types return false.
	invalidTypes := []string{"", "bogus", "action", "critical", "finding"}
	for _, typ := range invalidTypes {
		if ValidEventTypes[typ] {
			t.Errorf("ValidEventTypes[%q] should be false", typ)
		}
	}
}

// ---------------------------------------------------------------------------
// 5. ValidSeverities map completeness
// ---------------------------------------------------------------------------

func TestValidSeverities_Completeness(t *testing.T) {
	expectedSeverities := map[string]int{
		"info":     0,
		"warning":  1,
		"critical": 2,
	}

	if len(ValidSeverities) != 3 {
		t.Errorf("ValidSeverities has %d entries, want 3",
			len(ValidSeverities))
	}

	for sev, wantRank := range expectedSeverities {
		gotRank, exists := ValidSeverities[sev]
		if !exists {
			t.Errorf("ValidSeverities missing %q", sev)
			continue
		}
		if gotRank != wantRank {
			t.Errorf("ValidSeverities[%q] = %d, want %d",
				sev, gotRank, wantRank)
		}
	}

	// Verify no extra severities.
	for sev := range ValidSeverities {
		if _, expected := expectedSeverities[sev]; !expected {
			t.Errorf("ValidSeverities has unexpected severity %q", sev)
		}
	}

	// Verify ordering: info < warning < critical.
	if ValidSeverities["info"] >= ValidSeverities["warning"] {
		t.Error("info rank must be less than warning rank")
	}
	if ValidSeverities["warning"] >= ValidSeverities["critical"] {
		t.Error("warning rank must be less than critical rank")
	}
}

// ---------------------------------------------------------------------------
// 6. notifySeverityEmoji() — direct test per severity + unknown
// ---------------------------------------------------------------------------

func TestNotifySeverityEmoji_Table(t *testing.T) {
	tests := []struct {
		severity  string
		wantEmoji string
	}{
		{
			severity:  "critical",
			wantEmoji: "\xf0\x9f\x94\xb4", // red circle
		},
		{
			severity:  "warning",
			wantEmoji: "\xe2\x9a\xa0\xef\xb8\x8f", // warning sign
		},
		{
			severity:  "info",
			wantEmoji: "\xe2\x84\xb9\xef\xb8\x8f", // info
		},
		{
			severity:  "",
			wantEmoji: "\xe2\x84\xb9\xef\xb8\x8f", // default (info)
		},
		{
			severity:  "unknown",
			wantEmoji: "\xe2\x84\xb9\xef\xb8\x8f", // default (info)
		},
		{
			severity:  "debug",
			wantEmoji: "\xe2\x84\xb9\xef\xb8\x8f", // default (info)
		},
	}

	for _, tc := range tests {
		t.Run("severity_"+tc.severity, func(t *testing.T) {
			got := notifySeverityEmoji(tc.severity)
			if got != tc.wantEmoji {
				t.Errorf("notifySeverityEmoji(%q) = %q, want %q",
					tc.severity, got, tc.wantEmoji)
			}
		})
	}
}

// Verify each severity gets a distinct emoji (critical != warning != default).
func TestNotifySeverityEmoji_DistinctValues(t *testing.T) {
	critical := notifySeverityEmoji("critical")
	warning := notifySeverityEmoji("warning")
	info := notifySeverityEmoji("info")

	if critical == warning {
		t.Error("critical and warning should have different emojis")
	}
	if critical == info {
		t.Error("critical and info should have different emojis")
	}
	if warning == info {
		t.Error("warning and info should have different emojis")
	}
}

// ---------------------------------------------------------------------------
// Additional: Event factory functions produce valid event types
// ---------------------------------------------------------------------------

func TestEventFactory_AllTypesAreValid(t *testing.T) {
	events := []Event{
		ActionExecutedEvent("t", "s", "d"),
		ActionFailedEvent("t", "s", "d", "e"),
		ApprovalNeededEvent("t", "s", "d", "r"),
		FindingCriticalEvent("t", "d", "db"),
	}

	for _, evt := range events {
		if !ValidEventTypes[evt.Type] {
			t.Errorf("event factory produced invalid type %q", evt.Type)
		}
	}
}

func TestEventFactory_AllSeveritiesAreValid(t *testing.T) {
	events := []Event{
		ActionExecutedEvent("t", "s", "d"),
		ActionFailedEvent("t", "s", "d", "e"),
		ApprovalNeededEvent("t", "s", "d", "r"),
		FindingCriticalEvent("t", "d", "db"),
	}

	for _, evt := range events {
		if _, ok := ValidSeverities[evt.Severity]; !ok {
			t.Errorf("event factory %q produced invalid severity %q",
				evt.Type, evt.Severity)
		}
	}
}

// ---------------------------------------------------------------------------
// Additional: parseConfig edge cases
// ---------------------------------------------------------------------------

func TestParseConfig_Table(t *testing.T) {
	tests := []struct {
		name    string
		input   []byte
		wantLen int
	}{
		{
			name:    "nil input returns empty map",
			input:   nil,
			wantLen: 0,
		},
		{
			name:    "empty slice returns empty map",
			input:   []byte{},
			wantLen: 0,
		},
		{
			name:    "valid JSON object",
			input:   []byte(`{"webhook_url":"http://example.com"}`),
			wantLen: 1,
		},
		{
			name:    "invalid JSON returns empty map",
			input:   []byte(`not json`),
			wantLen: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseConfig(tc.input)
			if got == nil {
				t.Fatal("parseConfig returned nil, should always return a map")
			}
			if len(got) != tc.wantLen {
				t.Errorf("len(parseConfig) = %d, want %d", len(got), tc.wantLen)
			}
		})
	}
}

func TestParseConfig_ValidJSON_CorrectValues(t *testing.T) {
	input := []byte(`{"smtp_host":"mail.example.com","from":"a@b.com"}`)
	got := parseConfig(input)

	if got["smtp_host"] != "mail.example.com" {
		t.Errorf("smtp_host = %q, want 'mail.example.com'", got["smtp_host"])
	}
	if got["from"] != "a@b.com" {
		t.Errorf("from = %q, want 'a@b.com'", got["from"])
	}
}
