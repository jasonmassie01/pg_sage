package notify

import (
	"context"
	"fmt"
	"net"
	"net/smtp"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func connectTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, os.Getenv("SAGE_DATABASE_URL"))
	if err != nil {
		t.Skipf("test database unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("test database ping failed: %v", err)
	}
	if err := schema.Bootstrap(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("schema bootstrap failed: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func cleanupTestNotifyData(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, _ = pool.Exec(ctx,
		"DELETE FROM sage.notification_log WHERE subject LIKE 'phase2_test%'")
	_, _ = pool.Exec(ctx,
		"DELETE FROM sage.notification_rules WHERE channel_id IN "+
			"(SELECT id FROM sage.notification_channels "+
			"WHERE name LIKE 'phase2_test%')")
	_, _ = pool.Exec(ctx,
		"DELETE FROM sage.notification_channels WHERE name LIKE 'phase2_test%'")
}

func insertTestChannel(
	t *testing.T, pool *pgxpool.Pool,
	name, typ string, config map[string]string, enabled bool,
) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	cfgJSON := "{}"
	if len(config) > 0 {
		pairs := make([]string, 0, len(config))
		for k, v := range config {
			pairs = append(pairs,
				fmt.Sprintf(`"%s":"%s"`, k, v))
		}
		cfgJSON = "{" + strings.Join(pairs, ",") + "}"
	}

	var id int
	err := pool.QueryRow(ctx,
		`INSERT INTO sage.notification_channels
		 (name, type, config, enabled)
		 VALUES ($1, $2, $3::jsonb, $4)
		 RETURNING id`, name, typ, cfgJSON, enabled,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert test channel %q: %v", name, err)
	}
	return id
}

func insertTestRule(
	t *testing.T, pool *pgxpool.Pool,
	channelID int, event, minSeverity string, enabled bool,
) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	var id int
	err := pool.QueryRow(ctx,
		`INSERT INTO sage.notification_rules
		 (channel_id, event, min_severity, enabled)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id`, channelID, event, minSeverity, enabled,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert test rule: %v", err)
	}
	return id
}

type logCollector struct {
	entries []string
}

func (lc *logCollector) logFn(level, format string, args ...any) {
	lc.entries = append(lc.entries,
		fmt.Sprintf("[%s] %s", level,
			fmt.Sprintf(format, args...)))
}

// ---------------------------------------------------------------------------
// 1. Email: Type()
// ---------------------------------------------------------------------------

func TestPhase2_EmailSender_Type(t *testing.T) {
	s := NewEmailSender()
	if got := s.Type(); got != "email" {
		t.Fatalf("EmailSender.Type() = %q, want %q", got, "email")
	}
}

// ---------------------------------------------------------------------------
// 2. Slack: Type()
// ---------------------------------------------------------------------------

func TestPhase2_SlackSender_Type(t *testing.T) {
	s := NewSlackSender()
	if got := s.Type(); got != "slack" {
		t.Fatalf("SlackSender.Type() = %q, want %q", got, "slack")
	}
}

// ---------------------------------------------------------------------------
// 3. parseEmailConfig — expanded edge cases
// ---------------------------------------------------------------------------

func TestPhase2_ParseEmailConfig_HappyPath(t *testing.T) {
	ch := Channel{
		Name: "full-config",
		Config: map[string]string{
			"smtp_host": "mail.example.com",
			"smtp_port": "465",
			"smtp_user": "user@example.com",
			"smtp_pass": "secret",
			"from":      "noreply@example.com",
			"to":        "admin@example.com, ops@example.com",
		},
	}
	cfg, err := parseEmailConfig(ch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Host != "mail.example.com" {
		t.Errorf("Host = %q", cfg.Host)
	}
	if cfg.Port != "465" {
		t.Errorf("Port = %q, want 465", cfg.Port)
	}
	if cfg.User != "user@example.com" {
		t.Errorf("User = %q", cfg.User)
	}
	if cfg.Pass != "secret" {
		t.Errorf("Pass = %q", cfg.Pass)
	}
	if cfg.From != "noreply@example.com" {
		t.Errorf("From = %q", cfg.From)
	}
	if len(cfg.To) != 2 {
		t.Fatalf("To has %d recipients, want 2", len(cfg.To))
	}
	if cfg.To[0] != "admin@example.com" {
		t.Errorf("To[0] = %q", cfg.To[0])
	}
	if cfg.To[1] != "ops@example.com" {
		t.Errorf("To[1] = %q", cfg.To[1])
	}
}

func TestPhase2_ParseEmailConfig_DefaultPort(t *testing.T) {
	ch := Channel{
		Name: "default-port",
		Config: map[string]string{
			"smtp_host": "mail.example.com",
			"from":      "a@b.com",
			"to":        "c@d.com",
		},
	}
	cfg, err := parseEmailConfig(ch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != "587" {
		t.Errorf("default Port = %q, want 587", cfg.Port)
	}
}

func TestPhase2_ParseEmailConfig_EmptyPort(t *testing.T) {
	ch := Channel{
		Name: "empty-port",
		Config: map[string]string{
			"smtp_host": "mail.example.com",
			"smtp_port": "",
			"from":      "a@b.com",
			"to":        "c@d.com",
		},
	}
	cfg, err := parseEmailConfig(ch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != "587" {
		t.Errorf("empty smtp_port should default to 587, got %q",
			cfg.Port)
	}
}

func TestPhase2_ParseEmailConfig_NoUser(t *testing.T) {
	ch := Channel{
		Name: "no-user",
		Config: map[string]string{
			"smtp_host": "mail.example.com",
			"from":      "a@b.com",
			"to":        "c@d.com",
		},
	}
	cfg, err := parseEmailConfig(ch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.User != "" {
		t.Errorf("User = %q, want empty", cfg.User)
	}
	if cfg.Pass != "" {
		t.Errorf("Pass = %q, want empty", cfg.Pass)
	}
}

func TestPhase2_ParseEmailConfig_NilConfig(t *testing.T) {
	ch := Channel{Name: "nil-config", Config: nil}
	_, err := parseEmailConfig(ch)
	if err == nil {
		t.Fatal("expected error for nil config map")
	}
	if !strings.Contains(err.Error(), "smtp_host") {
		t.Errorf("error should mention smtp_host: %v", err)
	}
}

func TestPhase2_ParseEmailConfig_EmptyConfig(t *testing.T) {
	ch := Channel{Name: "empty-config", Config: map[string]string{}}
	_, err := parseEmailConfig(ch)
	if err == nil {
		t.Fatal("expected error for empty config map")
	}
	if !strings.Contains(err.Error(), "smtp_host") {
		t.Errorf("error should mention smtp_host: %v", err)
	}
}

func TestPhase2_ParseEmailConfig_MissingHost(t *testing.T) {
	ch := Channel{
		Name: "no-host",
		Config: map[string]string{
			"from": "a@b.com", "to": "c@d.com",
		},
	}
	_, err := parseEmailConfig(ch)
	if err == nil {
		t.Fatal("expected error for missing smtp_host")
	}
	if !strings.Contains(err.Error(), "smtp_host") {
		t.Errorf("error should mention smtp_host: %v", err)
	}
	if !strings.Contains(err.Error(), ch.Name) {
		t.Errorf("error should mention channel name %q: %v",
			ch.Name, err)
	}
}

func TestPhase2_ParseEmailConfig_MissingFrom(t *testing.T) {
	ch := Channel{
		Name: "no-from",
		Config: map[string]string{
			"smtp_host": "mail.example.com", "to": "c@d.com",
		},
	}
	_, err := parseEmailConfig(ch)
	if err == nil {
		t.Fatal("expected error for missing from")
	}
	if !strings.Contains(err.Error(), "from") {
		t.Errorf("error should mention from: %v", err)
	}
}

func TestPhase2_ParseEmailConfig_MissingTo(t *testing.T) {
	ch := Channel{
		Name: "no-to",
		Config: map[string]string{
			"smtp_host": "mail.example.com", "from": "a@b.com",
		},
	}
	_, err := parseEmailConfig(ch)
	if err == nil {
		t.Fatal("expected error for missing to")
	}
	if !strings.Contains(err.Error(), "to") {
		t.Errorf("error should mention to: %v", err)
	}
}

func TestPhase2_ParseEmailConfig_MultipleRecipients(t *testing.T) {
	ch := Channel{
		Name: "multi-to",
		Config: map[string]string{
			"smtp_host": "mail.example.com",
			"from":      "a@b.com",
			"to":        "x@y.com, a@b.com,  c@d.com  ,, e@f.com",
		},
	}
	cfg, err := parseEmailConfig(ch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.To) != 4 {
		t.Errorf("expected 4 recipients, got %d: %v",
			len(cfg.To), cfg.To)
	}
}

func TestPhase2_ParseEmailConfig_SingleRecipient(t *testing.T) {
	ch := Channel{
		Name: "single-to",
		Config: map[string]string{
			"smtp_host": "mail.example.com",
			"from":      "a@b.com",
			"to":        "admin@example.com",
		},
	}
	cfg, err := parseEmailConfig(ch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.To) != 1 || cfg.To[0] != "admin@example.com" {
		t.Errorf("To = %v, want [admin@example.com]", cfg.To)
	}
}

// ---------------------------------------------------------------------------
// 4. sendEmail — connection failure tests
// ---------------------------------------------------------------------------

func TestPhase2_SendEmail_ConnectionRefused(t *testing.T) {
	cfg := &emailConfig{
		Host: "127.0.0.1",
		Port: "1", // nothing listening
		From: "a@b.com",
		To:   []string{"c@d.com"},
	}
	evt := Event{
		Type: "action_executed", Severity: "info",
		Subject: "Test", Body: "Body",
	}
	err := sendEmail(t.Context(), cfg, evt)
	if err == nil {
		t.Fatal("expected error for connection refused")
	}
	if !strings.Contains(err.Error(), "tls dial") {
		t.Errorf("error should mention tls dial: %v", err)
	}
}

func TestPhase2_SendEmail_InvalidPort(t *testing.T) {
	cfg := &emailConfig{
		Host: "127.0.0.1",
		Port: "99999",
		From: "a@b.com",
		To:   []string{"c@d.com"},
	}
	evt := Event{
		Type: "action_executed", Severity: "info",
		Subject: "Test", Body: "Body",
	}
	err := sendEmail(t.Context(), cfg, evt)
	if err == nil {
		t.Fatal("expected error for invalid port")
	}
}

func TestPhase2_SendEmail_TLSHandshakeFails(t *testing.T) {
	// A plain TCP listener that immediately closes the connection
	// will cause the TLS handshake in sendEmail to fail.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, lnErr := ln.Accept()
		if lnErr != nil {
			return
		}
		conn.Close()
	}()

	host, port, _ := net.SplitHostPort(ln.Addr().String())
	cfg := &emailConfig{
		Host: host, Port: port,
		From: "a@b.com", To: []string{"c@d.com"},
	}
	evt := Event{
		Type: "action_executed", Severity: "info",
		Subject: "Test", Body: "Body",
	}
	err = sendEmail(t.Context(), cfg, evt)
	if err == nil {
		t.Fatal("expected error when TLS handshake fails")
	}
	if !strings.Contains(err.Error(), "tls dial") {
		t.Errorf("error should mention tls dial: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 5. authenticateSMTP — no user skips auth
// ---------------------------------------------------------------------------

func TestPhase2_AuthenticateSMTP_NoUser(t *testing.T) {
	cfg := &emailConfig{User: "", Pass: ""}
	// With empty user, the function returns nil immediately
	// without touching the smtp.Client, so nil client is safe.
	err := authenticateSMTP(nil, cfg)
	if err != nil {
		t.Fatalf("expected nil error for empty user, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 6. formatEmailMessage — verify structure
// ---------------------------------------------------------------------------

func TestPhase2_FormatEmailMessage_Structure(t *testing.T) {
	cfg := &emailConfig{
		From: "sender@example.com",
		To:   []string{"recip@example.com"},
	}
	evt := Event{
		Type: "finding_critical", Severity: "critical",
		Subject: "Disk full", Body: "90% used",
	}
	msg := formatEmailMessage(cfg, evt)

	checks := map[string]string{
		"From header":    "From: sender@example.com",
		"To header":      "To: recip@example.com",
		"Subject prefix": "Subject: [pg_sage] Disk full",
		"Content-Type":   "Content-Type: text/plain",
		"Body":           "90% used",
		"Event footer":   "Event: finding_critical",
		"Severity":       "Severity: critical",
		"CRLF":           "\r\n",
	}
	for label, want := range checks {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %s (%q)", label, want)
		}
	}
}

func TestPhase2_FormatEmailMessage_MultipleRecipients(t *testing.T) {
	cfg := &emailConfig{
		From: "sender@example.com",
		To:   []string{"a@b.com", "c@d.com", "e@f.com"},
	}
	evt := Event{
		Type: "action_executed", Severity: "info",
		Subject: "Test", Body: "Body",
	}
	msg := formatEmailMessage(cfg, evt)
	want := "To: a@b.com, c@d.com, e@f.com"
	if !strings.Contains(msg, want) {
		t.Errorf("To header should join with comma-space: %q", msg)
	}
}

func TestPhase2_FormatEmailMessage_EmptyBody(t *testing.T) {
	cfg := &emailConfig{
		From: "sender@example.com",
		To:   []string{"a@b.com"},
	}
	evt := Event{
		Type: "action_executed", Severity: "info",
		Subject: "Empty body test", Body: "",
	}
	msg := formatEmailMessage(cfg, evt)
	if !strings.Contains(msg, "Subject: [pg_sage] Empty body test") {
		t.Error("missing Subject header")
	}
	if !strings.Contains(msg, "Event: action_executed") {
		t.Error("missing Event footer")
	}
}

// ---------------------------------------------------------------------------
// 7. logDelivery — nil pool paths
// ---------------------------------------------------------------------------

func TestPhase2_LogDelivery_NilPool_Success(t *testing.T) {
	d := NewDispatcher(nil, func(string, string, ...any) {})
	evt := Event{
		Type: "action_executed", Severity: "info",
		Subject: "Test", Body: "Body",
	}
	err := d.logDelivery(t.Context(), 1, evt, "sent", "")
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
}

func TestPhase2_LogDelivery_NilPool_WithError(t *testing.T) {
	d := NewDispatcher(nil, func(string, string, ...any) {})
	evt := Event{
		Type: "action_executed", Severity: "info",
		Subject: "Test", Body: "Body",
	}
	err := d.logDelivery(t.Context(), 1, evt, "error", "mock failure")
	if err == nil {
		t.Fatal("expected error when errMsg is non-empty")
	}
	if !strings.Contains(err.Error(), "mock failure") {
		t.Errorf("error should contain errMsg: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 8. processRule — unit tests with mock sender (nil pool)
// ---------------------------------------------------------------------------

func TestPhase2_ProcessRule_SeverityFiltering(t *testing.T) {
	d := NewDispatcher(nil, func(string, string, ...any) {})
	ms := newMockSender("slack")
	d.RegisterSender(ms)

	rule := Rule{
		ID: 1, ChannelID: 1,
		Event: "action_executed", MinSeverity: "critical",
		Enabled: true,
	}
	evt := Event{
		Type: "action_executed", Severity: "info", Subject: "Test",
	}
	err := d.processRule(t.Context(), rule, evt)
	if err != nil {
		t.Fatalf("expected nil error (severity filtered), got: %v", err)
	}
	if ms.callCount() != 0 {
		t.Error("sender should not be called when severity is filtered")
	}
}

func TestPhase2_ProcessRule_NoSenderForType(t *testing.T) {
	pool := connectTestDB(t)
	if pool == nil {
		return
	}
	cleanupTestNotifyData(t, pool)
	t.Cleanup(func() { cleanupTestNotifyData(t, pool) })

	lc := &logCollector{}
	d := NewDispatcher(pool, lc.logFn)

	chID := insertTestChannel(t, pool,
		"phase2_test_no_sender", "webhook", nil, true)

	rule := Rule{
		ID: 99, ChannelID: chID,
		Event: "action_executed", MinSeverity: "info",
		Enabled: true,
	}
	evt := Event{
		Type: "action_executed", Severity: "info",
		Subject: "phase2_test_no_sender_subj",
	}
	err := d.processRule(t.Context(), rule, evt)
	if err == nil {
		t.Fatal("expected error when no sender registered")
	}
	if !strings.Contains(err.Error(), "no sender") {
		t.Errorf("error should mention 'no sender': %v", err)
	}
}

func TestPhase2_ProcessRule_DisabledChannel(t *testing.T) {
	pool := connectTestDB(t)
	if pool == nil {
		return
	}
	cleanupTestNotifyData(t, pool)
	t.Cleanup(func() { cleanupTestNotifyData(t, pool) })

	lc := &logCollector{}
	d := NewDispatcher(pool, lc.logFn)
	ms := newMockSender("slack")
	d.RegisterSender(ms)

	chID := insertTestChannel(t, pool,
		"phase2_test_disabled_ch", "slack",
		map[string]string{"webhook_url": "http://example.com"},
		false)

	rule := Rule{
		ID: 100, ChannelID: chID,
		Event: "action_executed", MinSeverity: "info",
		Enabled: true,
	}
	evt := Event{
		Type: "action_executed", Severity: "critical",
		Subject: "phase2_test_disabled_subj",
	}
	err := d.processRule(t.Context(), rule, evt)
	if err != nil {
		t.Fatalf("expected nil for disabled channel, got: %v", err)
	}
	if ms.callCount() != 0 {
		t.Error("sender should not be called for disabled channel")
	}
}

func TestPhase2_ProcessRule_SuccessfulSend(t *testing.T) {
	pool := connectTestDB(t)
	if pool == nil {
		return
	}
	cleanupTestNotifyData(t, pool)
	t.Cleanup(func() { cleanupTestNotifyData(t, pool) })

	lc := &logCollector{}
	d := NewDispatcher(pool, lc.logFn)
	ms := newMockSender("slack")
	d.RegisterSender(ms)

	chID := insertTestChannel(t, pool,
		"phase2_test_success_send", "slack",
		map[string]string{"webhook_url": "http://example.com"},
		true)

	rule := Rule{
		ID: 101, ChannelID: chID,
		Event: "action_executed", MinSeverity: "info",
		Enabled: true,
	}
	evt := Event{
		Type: "action_executed", Severity: "info",
		Subject: "phase2_test_success_subj", Body: "test body",
	}
	err := d.processRule(t.Context(), rule, evt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ms.callCount() != 1 {
		t.Errorf("expected 1 send call, got %d", ms.callCount())
	}
}

func TestPhase2_ProcessRule_SenderFails(t *testing.T) {
	pool := connectTestDB(t)
	if pool == nil {
		return
	}
	cleanupTestNotifyData(t, pool)
	t.Cleanup(func() { cleanupTestNotifyData(t, pool) })

	lc := &logCollector{}
	d := NewDispatcher(pool, lc.logFn)
	ms := newMockSender("slack")
	ms.failNext = true
	d.RegisterSender(ms)

	chID := insertTestChannel(t, pool,
		"phase2_test_sender_fails", "slack",
		map[string]string{"webhook_url": "http://example.com"},
		true)

	rule := Rule{
		ID: 102, ChannelID: chID,
		Event: "action_executed", MinSeverity: "info",
		Enabled: true,
	}
	evt := Event{
		Type: "action_executed", Severity: "info",
		Subject: "phase2_test_sender_fails_subj",
	}
	err := d.processRule(t.Context(), rule, evt)
	if err == nil {
		t.Fatal("expected error when sender fails")
	}
	if !strings.Contains(err.Error(), "send failed") {
		t.Errorf("error should contain 'send failed': %v", err)
	}
}

// ---------------------------------------------------------------------------
// 9. loadChannel — tests with real DB
// ---------------------------------------------------------------------------

func TestPhase2_LoadChannel_Exists(t *testing.T) {
	pool := connectTestDB(t)
	if pool == nil {
		return
	}
	cleanupTestNotifyData(t, pool)
	t.Cleanup(func() { cleanupTestNotifyData(t, pool) })

	d := NewDispatcher(pool, func(string, string, ...any) {})

	chID := insertTestChannel(t, pool,
		"phase2_test_load_ch", "email",
		map[string]string{
			"smtp_host": "mail.test.com",
			"from":      "test@test.com",
		}, true)

	ch, err := d.loadChannel(t.Context(), chID)
	if err != nil {
		t.Fatalf("loadChannel: %v", err)
	}
	if ch.ID != chID {
		t.Errorf("ch.ID = %d, want %d", ch.ID, chID)
	}
	if ch.Name != "phase2_test_load_ch" {
		t.Errorf("ch.Name = %q", ch.Name)
	}
	if ch.Type != "email" {
		t.Errorf("ch.Type = %q, want email", ch.Type)
	}
	if !ch.Enabled {
		t.Error("ch.Enabled should be true")
	}
	if ch.Config["smtp_host"] != "mail.test.com" {
		t.Errorf("Config[smtp_host] = %q", ch.Config["smtp_host"])
	}
	if ch.Config["from"] != "test@test.com" {
		t.Errorf("Config[from] = %q", ch.Config["from"])
	}
}

func TestPhase2_LoadChannel_NotFound(t *testing.T) {
	pool := connectTestDB(t)
	if pool == nil {
		return
	}
	d := NewDispatcher(pool, func(string, string, ...any) {})
	_, err := d.loadChannel(t.Context(), -99999)
	if err == nil {
		t.Fatal("expected error for non-existent channel")
	}
}

func TestPhase2_LoadChannel_EmptyConfig(t *testing.T) {
	pool := connectTestDB(t)
	if pool == nil {
		return
	}
	cleanupTestNotifyData(t, pool)
	t.Cleanup(func() { cleanupTestNotifyData(t, pool) })

	d := NewDispatcher(pool, func(string, string, ...any) {})
	chID := insertTestChannel(t, pool,
		"phase2_test_empty_cfg", "slack", nil, true)

	ch, err := d.loadChannel(t.Context(), chID)
	if err != nil {
		t.Fatalf("loadChannel: %v", err)
	}
	if ch.Config == nil {
		t.Fatal("Config should not be nil")
	}
}

func TestPhase2_LoadChannel_DisabledChannel(t *testing.T) {
	pool := connectTestDB(t)
	if pool == nil {
		return
	}
	cleanupTestNotifyData(t, pool)
	t.Cleanup(func() { cleanupTestNotifyData(t, pool) })

	d := NewDispatcher(pool, func(string, string, ...any) {})
	chID := insertTestChannel(t, pool,
		"phase2_test_disabled_load", "email",
		map[string]string{"smtp_host": "mail.test.com"}, false)

	ch, err := d.loadChannel(t.Context(), chID)
	if err != nil {
		t.Fatalf("loadChannel: %v", err)
	}
	if ch.Enabled {
		t.Error("channel should be disabled")
	}
}

// ---------------------------------------------------------------------------
// 10. loadMatchingRules — tests with real DB
// ---------------------------------------------------------------------------

func TestPhase2_LoadMatchingRules_NoRules(t *testing.T) {
	pool := connectTestDB(t)
	if pool == nil {
		return
	}
	d := NewDispatcher(pool, func(string, string, ...any) {})
	rules, err := d.loadMatchingRules(
		t.Context(), "nonexistent_event_type_phase2")
	if err != nil {
		t.Fatalf("loadMatchingRules: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("expected 0 rules, got %d", len(rules))
	}
}

func TestPhase2_LoadMatchingRules_MatchesEnabled(t *testing.T) {
	pool := connectTestDB(t)
	if pool == nil {
		return
	}
	cleanupTestNotifyData(t, pool)
	t.Cleanup(func() { cleanupTestNotifyData(t, pool) })

	d := NewDispatcher(pool, func(string, string, ...any) {})

	chID := insertTestChannel(t, pool,
		"phase2_test_rules_match", "slack",
		map[string]string{"webhook_url": "http://example.com"},
		true)

	insertTestRule(t, pool, chID,
		"action_executed", "info", true)
	insertTestRule(t, pool, chID,
		"action_executed", "warning", true)
	// Disabled — should NOT be returned.
	insertTestRule(t, pool, chID,
		"action_executed", "critical", false)

	rules, err := d.loadMatchingRules(
		t.Context(), "action_executed")
	if err != nil {
		t.Fatalf("loadMatchingRules: %v", err)
	}
	if len(rules) < 2 {
		t.Fatalf("expected at least 2 rules, got %d", len(rules))
	}
	for _, r := range rules {
		if !r.Enabled {
			t.Errorf("rule %d should be enabled", r.ID)
		}
		if r.Event != "action_executed" {
			t.Errorf("rule %d event = %q", r.ID, r.Event)
		}
	}
}

func TestPhase2_LoadMatchingRules_DifferentEventTypes(t *testing.T) {
	pool := connectTestDB(t)
	if pool == nil {
		return
	}
	cleanupTestNotifyData(t, pool)
	t.Cleanup(func() { cleanupTestNotifyData(t, pool) })

	d := NewDispatcher(pool, func(string, string, ...any) {})

	chID := insertTestChannel(t, pool,
		"phase2_test_rules_diff", "slack",
		map[string]string{"webhook_url": "http://example.com"},
		true)

	insertTestRule(t, pool, chID,
		"action_executed", "info", true)
	insertTestRule(t, pool, chID,
		"finding_critical", "critical", true)

	rules, err := d.loadMatchingRules(
		t.Context(), "action_executed")
	if err != nil {
		t.Fatalf("loadMatchingRules: %v", err)
	}
	for _, r := range rules {
		if r.Event != "action_executed" {
			t.Errorf("rule %d has event %q, expected action_executed",
				r.ID, r.Event)
		}
	}
}

func TestPhase2_LoadMatchingRules_FieldsPopulated(t *testing.T) {
	pool := connectTestDB(t)
	if pool == nil {
		return
	}
	cleanupTestNotifyData(t, pool)
	t.Cleanup(func() { cleanupTestNotifyData(t, pool) })

	d := NewDispatcher(pool, func(string, string, ...any) {})

	chID := insertTestChannel(t, pool,
		"phase2_test_rule_fields", "slack",
		map[string]string{"webhook_url": "http://example.com"},
		true)

	ruleID := insertTestRule(t, pool, chID,
		"approval_needed", "warning", true)

	rules, err := d.loadMatchingRules(
		t.Context(), "approval_needed")
	if err != nil {
		t.Fatalf("loadMatchingRules: %v", err)
	}

	var found bool
	for _, r := range rules {
		if r.ID == ruleID {
			found = true
			if r.ChannelID != chID {
				t.Errorf("ChannelID = %d, want %d",
					r.ChannelID, chID)
			}
			if r.Event != "approval_needed" {
				t.Errorf("Event = %q", r.Event)
			}
			if r.MinSeverity != "warning" {
				t.Errorf("MinSeverity = %q", r.MinSeverity)
			}
			if !r.Enabled {
				t.Error("Enabled should be true")
			}
			break
		}
	}
	if !found {
		t.Errorf("inserted rule %d not found", ruleID)
	}
}

// ---------------------------------------------------------------------------
// 11. Dispatch — end-to-end with real DB + mock sender
// ---------------------------------------------------------------------------

func TestPhase2_Dispatch_EndToEnd(t *testing.T) {
	pool := connectTestDB(t)
	if pool == nil {
		return
	}
	cleanupTestNotifyData(t, pool)
	t.Cleanup(func() { cleanupTestNotifyData(t, pool) })

	lc := &logCollector{}
	d := NewDispatcher(pool, lc.logFn)
	ms := newMockSender("slack")
	d.RegisterSender(ms)

	chID := insertTestChannel(t, pool,
		"phase2_test_dispatch_e2e", "slack",
		map[string]string{"webhook_url": "http://example.com"},
		true)
	insertTestRule(t, pool, chID,
		"action_executed", "info", true)

	evt := Event{
		Type: "action_executed", Severity: "info",
		Subject: "phase2_test_dispatch_subj",
		Body:    "dispatch test body",
	}
	err := d.Dispatch(t.Context(), evt)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if ms.callCount() < 1 {
		t.Errorf("expected at least 1 send call, got %d",
			ms.callCount())
	}
}

func TestPhase2_Dispatch_NoMatchingRules(t *testing.T) {
	pool := connectTestDB(t)
	if pool == nil {
		return
	}
	lc := &logCollector{}
	d := NewDispatcher(pool, lc.logFn)
	ms := newMockSender("slack")
	d.RegisterSender(ms)

	evt := Event{
		Type: "nonexistent_event_phase2_test", Severity: "info",
		Subject: "phase2_test_no_match",
	}
	err := d.Dispatch(t.Context(), evt)
	if err != nil {
		t.Fatalf("Dispatch should succeed with no rules: %v", err)
	}
	if ms.callCount() != 0 {
		t.Error("sender should not be called with no rules")
	}
}

func TestPhase2_Dispatch_SeverityFiltersAtRuleLevel(t *testing.T) {
	pool := connectTestDB(t)
	if pool == nil {
		return
	}
	cleanupTestNotifyData(t, pool)
	t.Cleanup(func() { cleanupTestNotifyData(t, pool) })

	lc := &logCollector{}
	d := NewDispatcher(pool, lc.logFn)
	ms := newMockSender("slack")
	d.RegisterSender(ms)

	chID := insertTestChannel(t, pool,
		"phase2_test_dispatch_sev", "slack",
		map[string]string{"webhook_url": "http://example.com"},
		true)
	insertTestRule(t, pool, chID,
		"action_executed", "critical", true)

	evt := Event{
		Type: "action_executed", Severity: "info",
		Subject: "phase2_test_dispatch_sev_subj",
	}
	err := d.Dispatch(t.Context(), evt)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if ms.callCount() != 0 {
		t.Error("sender should not be called when sev < min")
	}
}

func TestPhase2_Dispatch_MultipleRulesOneChannel(t *testing.T) {
	pool := connectTestDB(t)
	if pool == nil {
		return
	}
	cleanupTestNotifyData(t, pool)
	t.Cleanup(func() { cleanupTestNotifyData(t, pool) })

	lc := &logCollector{}
	d := NewDispatcher(pool, lc.logFn)
	ms := newMockSender("slack")
	d.RegisterSender(ms)

	chID := insertTestChannel(t, pool,
		"phase2_test_dispatch_multi", "slack",
		map[string]string{"webhook_url": "http://example.com"},
		true)
	insertTestRule(t, pool, chID,
		"finding_critical", "info", true)
	insertTestRule(t, pool, chID,
		"finding_critical", "warning", true)

	evt := Event{
		Type: "finding_critical", Severity: "critical",
		Subject: "phase2_test_dispatch_multi_subj",
	}
	err := d.Dispatch(t.Context(), evt)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if ms.callCount() < 2 {
		t.Errorf("expected at least 2 send calls, got %d",
			ms.callCount())
	}
}

func TestPhase2_Dispatch_SenderErrorLogged(t *testing.T) {
	pool := connectTestDB(t)
	if pool == nil {
		return
	}
	cleanupTestNotifyData(t, pool)
	t.Cleanup(func() { cleanupTestNotifyData(t, pool) })

	lc := &logCollector{}
	d := NewDispatcher(pool, lc.logFn)
	ms := newMockSender("slack")
	ms.failNext = true
	d.RegisterSender(ms)

	chID := insertTestChannel(t, pool,
		"phase2_test_dispatch_err", "slack",
		map[string]string{"webhook_url": "http://example.com"},
		true)
	insertTestRule(t, pool, chID,
		"action_failed", "info", true)

	evt := Event{
		Type: "action_failed", Severity: "warning",
		Subject: "phase2_test_dispatch_err_subj",
	}
	// Dispatch should NOT return error — per-rule errors are logged.
	err := d.Dispatch(t.Context(), evt)
	if err != nil {
		t.Fatalf("Dispatch should not return error: %v", err)
	}
	foundLog := false
	for _, entry := range lc.entries {
		if strings.Contains(entry, "ERROR") &&
			strings.Contains(entry, "dispatch") {
			foundLog = true
			break
		}
	}
	if !foundLog {
		t.Errorf("expected ERROR log for dispatch failure, "+
			"got: %v", lc.entries)
	}
}

func TestPhase2_Dispatch_ContextCancelled(t *testing.T) {
	pool := connectTestDB(t)
	if pool == nil {
		return
	}
	d := NewDispatcher(pool, func(string, string, ...any) {})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	// May or may not error depending on timing. Should not panic.
	_ = d.Dispatch(ctx, Event{
		Type: "action_executed", Severity: "info",
		Subject: "cancelled test",
	})
}

// ---------------------------------------------------------------------------
// 12. logDelivery — with real DB
// ---------------------------------------------------------------------------

func TestPhase2_LogDelivery_WithPool_Success(t *testing.T) {
	pool := connectTestDB(t)
	if pool == nil {
		return
	}
	cleanupTestNotifyData(t, pool)
	t.Cleanup(func() { cleanupTestNotifyData(t, pool) })

	lc := &logCollector{}
	d := NewDispatcher(pool, lc.logFn)

	chID := insertTestChannel(t, pool,
		"phase2_test_logdel_ok", "slack",
		map[string]string{"webhook_url": "http://example.com"},
		true)

	evt := Event{
		Type: "action_executed", Severity: "info",
		Subject: "phase2_test_logdel_ok_subj", Body: "logged",
	}
	err := d.logDelivery(t.Context(), chID, evt, "sent", "")
	if err != nil {
		t.Fatalf("logDelivery: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var count int
	err = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM sage.notification_log
		 WHERE channel_id = $1
		 AND subject = $2
		 AND status = 'sent'`,
		chID, "phase2_test_logdel_ok_subj").Scan(&count)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if count == 0 {
		t.Error("expected log entry in notification_log")
	}
}

func TestPhase2_LogDelivery_WithPool_Error(t *testing.T) {
	pool := connectTestDB(t)
	if pool == nil {
		return
	}
	cleanupTestNotifyData(t, pool)
	t.Cleanup(func() { cleanupTestNotifyData(t, pool) })

	lc := &logCollector{}
	d := NewDispatcher(pool, lc.logFn)

	chID := insertTestChannel(t, pool,
		"phase2_test_logdel_err", "slack",
		map[string]string{"webhook_url": "http://example.com"},
		true)

	evt := Event{
		Type: "action_failed", Severity: "warning",
		Subject: "phase2_test_logdel_err_subj", Body: "error body",
	}
	err := d.logDelivery(t.Context(), chID, evt,
		"error", "delivery timed out")
	if err == nil {
		t.Fatal("expected error when errMsg is non-empty")
	}
	if !strings.Contains(err.Error(), "delivery timed out") {
		t.Errorf("error should contain errMsg: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var count int
	err = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM sage.notification_log
		 WHERE channel_id = $1
		 AND subject = $2
		 AND status = 'error'`,
		chID, "phase2_test_logdel_err_subj").Scan(&count)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if count == 0 {
		t.Error("expected error log entry in notification_log")
	}
}

// ---------------------------------------------------------------------------
// 13. SendDirect — expanded tests
// ---------------------------------------------------------------------------

func TestPhase2_SendDirect_NoSenderForType(t *testing.T) {
	d := NewDispatcher(nil, func(string, string, ...any) {})
	ch := Channel{ID: 1, Name: "test", Type: "nonexistent_type"}
	evt := Event{
		Type: "action_executed", Severity: "info", Subject: "Test",
	}
	err := d.SendDirect(t.Context(), ch, evt)
	if err == nil {
		t.Fatal("expected error for unregistered sender type")
	}
	if !strings.Contains(err.Error(), "no sender") {
		t.Errorf("error should mention 'no sender': %v", err)
	}
	if !strings.Contains(err.Error(), "nonexistent_type") {
		t.Errorf("error should mention the type: %v", err)
	}
}

func TestPhase2_SendDirect_SenderSuccess_NilPool(t *testing.T) {
	d := NewDispatcher(nil, func(string, string, ...any) {})
	ms := newMockSender("slack")
	d.RegisterSender(ms)

	ch := Channel{ID: 1, Name: "test", Type: "slack"}
	evt := Event{
		Type: "action_executed", Severity: "info",
		Subject: "Test", Body: "Body",
	}
	err := d.SendDirect(t.Context(), ch, evt)
	if err != nil {
		t.Fatalf("expected nil error: %v", err)
	}
	if ms.callCount() != 1 {
		t.Errorf("expected 1 call, got %d", ms.callCount())
	}
}

func TestPhase2_SendDirect_SenderFails_NilPool(t *testing.T) {
	d := NewDispatcher(nil, func(string, string, ...any) {})
	ms := newMockSender("email")
	ms.failNext = true
	d.RegisterSender(ms)

	ch := Channel{ID: 1, Name: "test", Type: "email"}
	evt := Event{
		Type: "action_failed", Severity: "warning", Subject: "Fail",
	}
	err := d.SendDirect(t.Context(), ch, evt)
	if err == nil {
		t.Fatal("expected error when sender fails")
	}
	if !strings.Contains(err.Error(), "send failed") {
		t.Errorf("error should contain 'send failed': %v", err)
	}
}

func TestPhase2_SendDirect_WithPool(t *testing.T) {
	pool := connectTestDB(t)
	if pool == nil {
		return
	}
	cleanupTestNotifyData(t, pool)
	t.Cleanup(func() { cleanupTestNotifyData(t, pool) })

	lc := &logCollector{}
	d := NewDispatcher(pool, lc.logFn)
	ms := newMockSender("slack")
	d.RegisterSender(ms)

	chID := insertTestChannel(t, pool,
		"phase2_test_senddirect_pool", "slack",
		map[string]string{"webhook_url": "http://example.com"},
		true)

	ch := Channel{
		ID: chID, Name: "phase2_test_senddirect_pool",
		Type: "slack",
	}
	evt := Event{
		Type: "action_executed", Severity: "info",
		Subject: "phase2_test_senddirect_subj",
		Body:    "direct send test",
	}
	err := d.SendDirect(t.Context(), ch, evt)
	if err != nil {
		t.Fatalf("SendDirect: %v", err)
	}
	if ms.callCount() != 1 {
		t.Errorf("expected 1 call, got %d", ms.callCount())
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var count int
	err = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM sage.notification_log
		 WHERE channel_id = $1 AND subject = $2`,
		chID, "phase2_test_senddirect_subj").Scan(&count)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if count == 0 {
		t.Error("expected log entry after SendDirect")
	}
}

// ---------------------------------------------------------------------------
// 14. Sender interface conformance
// ---------------------------------------------------------------------------

func TestPhase2_EmailSender_ImplementsSender(t *testing.T) {
	var _ Sender = NewEmailSender()
}

func TestPhase2_SlackSender_ImplementsSender(t *testing.T) {
	var _ Sender = NewSlackSender()
}

func TestPhase2_PagerDutySender_ImplementsSender(t *testing.T) {
	var _ Sender = NewPagerDutySender()
}

// ---------------------------------------------------------------------------
// 15. NewDispatcher — verify initial state
// ---------------------------------------------------------------------------

func TestPhase2_NewDispatcher_InitialState(t *testing.T) {
	lc := &logCollector{}
	d := NewDispatcher(nil, lc.logFn)

	if d.pool != nil {
		t.Error("expected nil pool")
	}
	if d.senders == nil {
		t.Fatal("senders map should be initialized")
	}
	if len(d.senders) != 0 {
		t.Errorf("expected 0 senders, got %d", len(d.senders))
	}
	if d.logFn == nil {
		t.Error("logFn should be set")
	}
}

// ---------------------------------------------------------------------------
// 16. setSMTPEnvelope — via fake SMTP server
// ---------------------------------------------------------------------------

func fakeSMTPServer(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			conn, lnErr := ln.Accept()
			if lnErr != nil {
				return
			}
			go handleFakeSMTP(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func handleFakeSMTP(conn net.Conn) {
	defer conn.Close()
	fmt.Fprintf(conn, "220 fake SMTP\r\n")

	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		line := string(buf[:n])

		switch {
		case strings.HasPrefix(line, "EHLO"),
			strings.HasPrefix(line, "HELO"):
			fmt.Fprintf(conn, "250 OK\r\n")
		case strings.HasPrefix(line, "MAIL FROM"):
			fmt.Fprintf(conn, "250 OK\r\n")
		case strings.HasPrefix(line, "RCPT TO"):
			fmt.Fprintf(conn, "250 OK\r\n")
		case strings.HasPrefix(line, "DATA"):
			fmt.Fprintf(conn, "354 Go ahead\r\n")
			for {
				n2, err2 := conn.Read(buf)
				if err2 != nil {
					return
				}
				if strings.Contains(
					string(buf[:n2]), "\r\n.\r\n") {
					break
				}
			}
			fmt.Fprintf(conn, "250 OK\r\n")
		case strings.HasPrefix(line, "QUIT"):
			fmt.Fprintf(conn, "221 Bye\r\n")
			return
		default:
			fmt.Fprintf(conn, "502 Not implemented\r\n")
		}
	}
}

func TestPhase2_SetSMTPEnvelope_ViaFakeServer(t *testing.T) {
	addr, cleanup := fakeSMTPServer(t)
	defer cleanup()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	host, _, _ := net.SplitHostPort(addr)
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		t.Fatalf("smtp.NewClient: %v", err)
	}
	defer client.Close()

	cfg := &emailConfig{
		From: "sender@example.com",
		To: []string{
			"recip1@example.com",
			"recip2@example.com",
		},
	}
	err = setSMTPEnvelope(client, cfg)
	if err != nil {
		t.Fatalf("setSMTPEnvelope: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 17. writeMessage — via fake SMTP server
// ---------------------------------------------------------------------------

func TestPhase2_WriteMessage_ViaFakeServer(t *testing.T) {
	addr, cleanup := fakeSMTPServer(t)
	defer cleanup()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	host, _, _ := net.SplitHostPort(addr)
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		t.Fatalf("smtp.NewClient: %v", err)
	}
	defer client.Close()

	cfg := &emailConfig{
		From: "sender@example.com",
		To:   []string{"recip@example.com"},
	}
	evt := Event{
		Type: "action_executed", Severity: "info",
		Subject: "Test writeMessage", Body: "This is a test body",
	}

	if err := setSMTPEnvelope(client, cfg); err != nil {
		t.Fatalf("setSMTPEnvelope: %v", err)
	}
	err = writeMessage(client, cfg, evt)
	if err != nil {
		t.Fatalf("writeMessage: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 18. authenticateSMTP — with credentials on a fake server
// ---------------------------------------------------------------------------

func TestPhase2_AuthenticateSMTP_WithCreds_NoAuthSupport(
	t *testing.T,
) {
	addr, cleanup := fakeSMTPServer(t)
	defer cleanup()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	host, _, _ := net.SplitHostPort(addr)
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		t.Fatalf("smtp.NewClient: %v", err)
	}
	defer client.Close()

	cfg := &emailConfig{
		Host: host,
		User: "user@example.com",
		Pass: "password",
	}
	err = authenticateSMTP(client, cfg)
	if err == nil {
		t.Fatal("expected error: fake server has no AUTH")
	}
	if !strings.Contains(err.Error(), "smtp auth") {
		t.Errorf("error should mention smtp auth: %v", err)
	}
}
