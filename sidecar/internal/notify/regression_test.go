package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// --- G7-B05: dispatcher reads rules through an injectable store -------

type fakeRuleStore struct {
	rules   []Rule
	channel Channel
	logged  []string
}

func (f *fakeRuleStore) MatchingRules(
	_ context.Context, eventType string,
) ([]Rule, error) {
	var out []Rule
	for _, r := range f.rules {
		if r.Event == eventType {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeRuleStore) Channel(_ context.Context, _ int) (*Channel, error) {
	ch := f.channel
	return &ch, nil
}

func (f *fakeRuleStore) LogDelivery(
	_ context.Context, _ int, evt Event, status, errMsg string,
) error {
	f.logged = append(f.logged, evt.Type+"|"+status+"|"+errMsg)
	return nil
}

type recordingSender struct {
	typ   string
	err   error
	calls []Event
}

func (s *recordingSender) Type() string { return s.typ }
func (s *recordingSender) Send(_ context.Context, _ Channel, evt Event) error {
	s.calls = append(s.calls, evt)
	return s.err
}

func TestDispatcher_ReadsRulesFromInjectedStore(t *testing.T) {
	store := &fakeRuleStore{
		rules: []Rule{{ID: 1, ChannelID: 7, Event: "finding_critical",
			MinSeverity: "info", Enabled: true}},
		channel: Channel{ID: 7, Name: "ops", Type: "fake", Enabled: true},
	}
	sender := &recordingSender{typ: "fake"}
	d := NewDispatcherWithStore(store, func(string, string, ...any) {})
	d.RegisterSender(sender)
	err := d.Dispatch(context.Background(),
		FindingCriticalEvent("xid wraparound", "d", "billing"))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(sender.calls) != 1 {
		t.Fatalf("sender calls = %d, want 1", len(sender.calls))
	}
	if len(store.logged) != 1 || store.logged[0] != "finding_critical|sent|" {
		t.Fatalf("logged = %v", store.logged)
	}
}

// --- G7-B06: every event's own severity can satisfy its default rule --

func TestRuleCanFire_FixedEventSeverities(t *testing.T) {
	if RuleCanFire("action_executed", "warning") {
		t.Fatal("action_executed (info) must not satisfy min_severity=warning")
	}
	for event := range ValidEventTypes {
		if !RuleCanFire(event, DefaultMinSeverity(event)) {
			t.Fatalf("default rule for %s can never fire", event)
		}
	}
	if DefaultMinSeverity("action_executed") != "info" {
		t.Fatalf("default for action_executed = %q, want info",
			DefaultMinSeverity("action_executed"))
	}
	if RuleCanFire("no_such_event", "info") {
		t.Fatal("unknown event reported as fireable")
	}
}

// Incident events carry the incident's severity, so a critical-only rule
// must be fireable and the default rule must not require critical.
func TestRuleCanFire_VariableSeverityIncidentEvents(t *testing.T) {
	for _, event := range []string{
		"incident_detected", "incident_escalated", "incident_resolved",
	} {
		if !RuleCanFire(event, "critical") {
			t.Fatalf("%s rule with min_severity=critical can never fire", event)
		}
		if got := DefaultMinSeverity(event); got != "warning" {
			t.Fatalf("default min severity for %s = %q, want warning", event, got)
		}
	}
	critical := IncidentDetectedEvent(IncidentInfo{Severity: "critical"})
	if !SeverityMeetsMin(critical.Severity, "critical") {
		t.Fatalf("critical incident event severity = %q", critical.Severity)
	}
}

// --- G7-B08 / G7-B16: email transport ---------------------------------

func testEmailChannel(port string, extra map[string]string) Channel {
	cfg := map[string]string{
		"smtp_host": "127.0.0.1", "smtp_port": port,
		"smtp_user": "u", "smtp_pass": "p",
		"from": "sage@example.com", "to": "ops@example.com",
	}
	for k, v := range extra {
		cfg[k] = v
	}
	return Channel{ID: 1, Name: "mail", Type: "email", Config: cfg}
}

func testEmailSender(f *fakeSMTP, timeout time.Duration) *EmailSender {
	return &EmailSender{
		policy:  TargetPolicy{AllowPrivate: true},
		tlsBase: &tls.Config{RootCAs: f.roots},
		timeout: timeout,
	}
}

func TestEmailSender_STARTTLSSubmission(t *testing.T) {
	f := startFakeSMTP(t, smtpStartTLS)
	evt := Event{Type: "action_failed", Severity: "warning",
		Subject: "disk full", Body: "details"}
	err := testEmailSender(f, 5*time.Second).Send(
		context.Background(), testEmailChannel(f.port(), nil), evt)
	if err != nil {
		t.Fatalf("Send over STARTTLS: %v", err)
	}
	select {
	case data := <-f.data:
		if !strings.Contains(data, "Subject: [pg_sage] disk full") {
			t.Fatalf("delivered message = %q", data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no message delivered")
	}
}

func TestEmailSender_RefusesServerWithoutSTARTTLS(t *testing.T) {
	f := startFakeSMTP(t, smtpPlainOnly)
	err := testEmailSender(f, 5*time.Second).Send(context.Background(),
		testEmailChannel(f.port(), nil), Event{Subject: "x"})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("err = %v, want STARTTLS refusal", err)
	}
	select {
	case <-f.data:
		t.Fatal("message delivered over plaintext")
	default:
	}
}

func TestEmailSender_ImplicitTLS(t *testing.T) {
	f := startFakeSMTP(t, smtpImplicitTLS)
	err := testEmailSender(f, 5*time.Second).Send(context.Background(),
		testEmailChannel(f.port(), map[string]string{"smtp_tls": "implicit"}),
		Event{Subject: "implicit"})
	if err != nil {
		t.Fatalf("Send over implicit TLS: %v", err)
	}
}

func TestEmailSender_StalledServerTimesOut(t *testing.T) {
	f := startFakeSMTP(t, smtpSilent)
	start := time.Now()
	err := testEmailSender(f, 300*time.Millisecond).Send(
		context.Background(), testEmailChannel(f.port(), nil),
		Event{Subject: "x"})
	if err == nil {
		t.Fatal("Send to stalled server succeeded")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Send blocked for %s, want < 2s", elapsed)
	}
}

// --- G7-B30: header injection -------------------------------------------

func TestFormatEmailMessage_NoHeaderInjection(t *testing.T) {
	cfg := &emailConfig{From: "sage@example.com", To: []string{"ops@example.com"}}
	msg := formatEmailMessage(cfg, Event{
		Subject: "idx \"a\r\nBcc: victim@example.com\" bloated",
		Body:    "body", Type: "finding_critical", Severity: "critical",
	})
	headers := msg[:strings.Index(msg, "\r\n\r\n")]
	for _, line := range strings.Split(headers, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "bcc:") {
			t.Fatalf("injected header line %q in %q", line, headers)
		}
	}
	if !strings.Contains(headers, "\r\nDate: ") ||
		!strings.Contains(headers, "\r\nMessage-ID: <") {
		t.Fatalf("missing Date/Message-ID headers: %q", headers)
	}
}

// --- G7-B10: secrets in error strings -----------------------------------

func TestSlackSend_ErrorDoesNotLeakWebhookPath(t *testing.T) {
	s := NewSlackSenderWithPolicy(TargetPolicy{AllowPrivate: true})
	ch := Channel{Name: "s", Config: map[string]string{
		"webhook_url": "http://127.0.0.1:1/services/T0/B0/SUPERSECRETTOKEN",
	}}
	err := s.Send(context.Background(), ch, Event{Subject: "x"})
	if err == nil {
		t.Fatal("expected connection error")
	}
	if strings.Contains(err.Error(), "SUPERSECRETTOKEN") ||
		strings.Contains(err.Error(), "/services/") {
		t.Fatalf("error leaks webhook path: %v", err)
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("error lost the host for diagnosis: %v", err)
	}
}

func TestDispatcher_LogsRedactedErrors(t *testing.T) {
	store := &fakeRuleStore{
		rules: []Rule{{ID: 1, ChannelID: 7, Event: "action_failed",
			MinSeverity: "info"}},
		channel: Channel{ID: 7, Type: "fake", Enabled: true},
	}
	sender := &recordingSender{typ: "fake", err: &urlErrorForTest{}}
	d := NewDispatcherWithStore(store, func(string, string, ...any) {})
	d.RegisterSender(sender)
	_ = d.Dispatch(context.Background(), ActionFailedEvent("t", "s", "db", "e"))
	if len(store.logged) != 1 || strings.Contains(store.logged[0], "TOKEN") {
		t.Fatalf("logged = %v", store.logged)
	}
}

type urlErrorForTest struct{}

func (*urlErrorForTest) Error() string {
	return `Post "https://hooks.slack.com/services/T0/B0/TOKEN": EOF`
}

func TestRedactURLs_InFreeText(t *testing.T) {
	got := RedactURLs(`Post "https://hooks.slack.com/services/T/B/TOKEN?x=1": EOF`)
	if strings.Contains(got, "TOKEN") || !strings.Contains(got, "hooks.slack.com") {
		t.Fatalf("RedactURLs = %q", got)
	}
}

// --- G7-B14: PagerDuty dedup per finding + resolve ----------------------

func TestPagerDuty_DedupKeyPerFindingAndResolve(t *testing.T) {
	ch := Channel{Config: map[string]string{}}
	a := FindingCriticalEvent("XID wraparound on orders", "d", "db1")
	b := FindingCriticalEvent("Replication slot inactive: s1", "d", "db1")
	keyA := pdDedupKey(t, ch, a)
	keyB := pdDedupKey(t, ch, b)
	if keyA == keyB {
		t.Fatalf("distinct findings share dedup key %q", keyA)
	}
	resolved := ResolvedEvent(a)
	raw, err := buildPagerDutyPayload(ch, resolved, "rk")
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	var ev pdEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ev.EventAction != "resolve" || ev.DedupKey != keyA {
		t.Fatalf("resolve event = %+v, want action resolve key %q", ev, keyA)
	}
}

func pdDedupKey(t *testing.T, ch Channel, evt Event) string {
	t.Helper()
	raw, err := buildPagerDutyPayload(ch, evt, "rk")
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	var ev pdEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return ev.DedupKey
}

// --- G7-B15 / G7-B31: Slack block limits and mrkdwn escaping -------------

func TestSlackPayload_TruncatesAndEscapes(t *testing.T) {
	evt := Event{
		Type: "finding_critical", Severity: "critical",
		Subject: strings.Repeat("é", 400),
		Body:    "<!channel> & <https://evil.example|click> " + strings.Repeat("x", 5000),
	}
	raw, err := buildSlackPayload(evt)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	var p struct {
		Blocks []struct {
			Type string `json:"type"`
			Text struct {
				Text string `json:"text"`
			} `json:"text"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	header, section := p.Blocks[0].Text.Text, p.Blocks[1].Text.Text
	if n := utf8.RuneCountInString(header); n > 150 || !utf8.ValidString(header) {
		t.Fatalf("header runes = %d valid=%v", n, utf8.ValidString(header))
	}
	if n := utf8.RuneCountInString(section); n > 3000 {
		t.Fatalf("section runes = %d, want <= 3000", n)
	}
	if strings.Contains(section, "<!channel>") ||
		strings.Contains(section, "<https://evil") {
		t.Fatalf("mrkdwn control sequences not escaped: %q", section[:80])
	}
	if !strings.HasPrefix(section, "&lt;!channel&gt; &amp; ") {
		t.Fatalf("escaped prefix = %q", section[:40])
	}
}

// --- G7-B21: target validation and guarded dialing ----------------------

func TestTargetPolicy_ValidateURL(t *testing.T) {
	deny := []string{
		"http://169.254.169.254/latest/meta-data",
		"https://169.254.169.254/x",
		"file:///etc/passwd",
		"gopher://hooks.slack.com/x",
		"http://127.0.0.1:8080/x",
		"https://10.0.0.5/x",
		"https://192.168.1.10/x",
		"https://[::1]/x",
		"https://localhost/x",
		"https://metadata.google.internal/x",
		"https:///nohost",
	}
	strict := TargetPolicy{}
	for _, u := range deny {
		if err := strict.ValidateURL(u); err == nil {
			t.Errorf("ValidateURL(%q) accepted", u)
		}
	}
	if err := strict.ValidateURL("https://hooks.slack.com/services/a"); err != nil {
		t.Fatalf("public https rejected: %v", err)
	}
	open := TargetPolicy{AllowPrivate: true}
	if err := open.ValidateURL("http://127.0.0.1:8080/x"); err != nil {
		t.Fatalf("AllowPrivate rejected loopback: %v", err)
	}
	if err := open.ValidateURL("http://169.254.169.254/x"); err == nil {
		t.Fatal("AllowPrivate must still reject link-local metadata")
	}
}

func TestSlackSender_DefaultPolicyBlocksLoopbackAtDial(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
	defer srv.Close()
	ch := Channel{Name: "s", Config: map[string]string{"webhook_url": srv.URL}}
	err := NewSlackSender().Send(context.Background(), ch, Event{Subject: "x"})
	if err == nil || hits.Load() != 0 {
		t.Fatalf("err=%v hits=%d, want blocked before connect", err, hits.Load())
	}
}

// --- G7-B20: channel secrets at rest ------------------------------------

func TestSealOpenSecrets(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	plain := map[string]string{
		"webhook_url": "https://hooks.slack.com/services/T/B/TOKEN",
		"smtp_pass":   "hunter2", "to": "ops@example.com",
	}
	sealed, err := SealSecrets(plain, key)
	if err != nil {
		t.Fatalf("SealSecrets: %v", err)
	}
	for _, k := range []string{"webhook_url", "smtp_pass"} {
		if !strings.HasPrefix(sealed[k], sealedPrefix) ||
			strings.Contains(sealed[k], "TOKEN") ||
			strings.Contains(sealed[k], "hunter2") {
			t.Fatalf("%s not sealed: %q", k, sealed[k])
		}
	}
	if sealed["to"] != "ops@example.com" {
		t.Fatalf("non-secret changed: %q", sealed["to"])
	}
	if plain["smtp_pass"] != "hunter2" {
		t.Fatal("SealSecrets mutated its input")
	}
	opened, err := OpenSecrets(sealed, key)
	if err != nil || opened["webhook_url"] != plain["webhook_url"] ||
		opened["smtp_pass"] != "hunter2" {
		t.Fatalf("OpenSecrets = %v, %v", opened, err)
	}
	if _, err := OpenSecrets(sealed, bytes.Repeat([]byte{8}, 32)); err == nil {
		t.Fatal("OpenSecrets with wrong key succeeded")
	}
	if _, err := OpenSecrets(sealed, nil); err == nil {
		t.Fatal("OpenSecrets without key succeeded on sealed values")
	}
	if !HasPlaintextSecrets(plain) || HasPlaintextSecrets(sealed) {
		t.Fatal("HasPlaintextSecrets misclassified configs")
	}
}
