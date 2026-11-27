package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// sre-08: a diagnosis follow-up goes only where the incident page went,
// threads under the Telegram page and never reaches PagerDuty.

func lockConclusion() ConclusionInfo {
	return ConclusionInfo{IncidentID: "inc-1", InvestigationID: "inv-1",
		CaseID: "incident:orders:lock", Database: "orders", Family: "lock_blocking",
		State: "concluded", RootLabel: "idle-in-transaction holder",
		Reason: "root blocker pid 7 is idle", Severity: "critical"}
}

func TestInvestigationConcludedEvent_Body(t *testing.T) {
	evt := InvestigationConcludedEvent(lockConclusion())
	want := "Diagnosis for incident inc-1 on orders: lock_blocking - " +
		"idle-in-transaction holder: root blocker pid 7 is idle. Case incident:orders:lock."
	if evt.Body != want {
		t.Fatalf("body = %q\nwant   %q", evt.Body, want)
	}
	if EventInvestigationConcluded != "investigation_concluded" {
		t.Fatalf("event type = %q", EventInvestigationConcluded)
	}
	if evt.Type != EventInvestigationConcluded || evt.Severity != "critical" ||
		evt.RefID != "inc-1" || evt.Subject == "" ||
		evt.Data["incident_id"] != "inc-1" || evt.Data["investigation_id"] != "inv-1" {
		t.Fatalf("event = %+v", evt)
	}
}

func TestInvestigationConcludedEvent_Inconclusive(t *testing.T) {
	info := lockConclusion()
	info.State, info.RootLabel, info.Reason = "inconclusive", "", "lock graph empty."
	evt := InvestigationConcludedEvent(info)
	want := "Diagnosis for incident inc-1 on orders: lock_blocking - inconclusive: " +
		"lock graph empty. Case incident:orders:lock."
	if evt.Body != want {
		t.Fatalf("body = %q\nwant   %q", evt.Body, want)
	}
	info.Reason = ""
	if got := InvestigationConcludedEvent(info).Body; got !=
		"Diagnosis for incident inc-1 on orders: lock_blocking - inconclusive. "+
			"Case incident:orders:lock." {
		t.Fatalf("body without reason = %q", got)
	}
}

func TestInvestigationConcludedEvent_SeverityFollowsTheIncident(t *testing.T) {
	for in, want := range map[string]string{"critical": "critical", "warning": "warning",
		"info": "info", "": "warning", "doom": "warning"} {
		info := lockConclusion()
		info.Severity = in
		if got := InvestigationConcludedEvent(info).Severity; got != want {
			t.Errorf("severity %q -> %q, want %q", in, got, want)
		}
	}
}

// No rule can be created for the follow-up: it is not a rule event.
func TestInvestigationConcluded_IsNotARuleEvent(t *testing.T) {
	if ValidEventTypes[EventInvestigationConcluded] {
		t.Fatal("investigation_concluded must not be selectable in notification rules")
	}
}

func TestIncidentEvents_CarryTheIncidentRef(t *testing.T) {
	info := IncidentInfo{ID: "inc-9", Database: "orders", Severity: "warning"}
	for _, evt := range []Event{IncidentDetectedEvent(info), IncidentEscalatedEvent(info),
		IncidentResolvedEvent(info)} {
		if evt.RefID != "inc-9" {
			t.Errorf("%s ref = %q, want inc-9", evt.Type, evt.RefID)
		}
	}
}

// pageStore is a RuleStore with recorded deliveries and incident pages.
type pageStore struct {
	mu         sync.Mutex
	channels   map[int]*Channel
	pages      map[string][]IncidentPage
	pagesErr   error
	deliveries []Delivery
}

func (s *pageStore) MatchingRules(context.Context, string) ([]Rule, error) { return nil, nil }

func (s *pageStore) Channel(_ context.Context, id int) (*Channel, error) {
	if ch, ok := s.channels[id]; ok {
		return ch, nil
	}
	return nil, errors.New("channel not found")
}

func (s *pageStore) LogDelivery(ctx context.Context, channelID int, evt Event, status,
	errMsg string) error {
	return s.RecordDelivery(ctx, Delivery{ChannelID: channelID, Event: evt, Status: status,
		Error: errMsg})
}

func (s *pageStore) RecordDelivery(_ context.Context, d Delivery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deliveries = append(s.deliveries, d)
	return nil
}

func (s *pageStore) IncidentPages(_ context.Context, id string) ([]IncidentPage, error) {
	return s.pages[id], s.pagesErr
}

func (s *pageStore) recorded() []Delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Delivery(nil), s.deliveries...)
}

func followUpFixture() (*pageStore, *Dispatcher, map[string]*mockSender) {
	st := &pageStore{channels: map[int]*Channel{
		1: {ID: 1, Name: "ops-slack", Type: "slack", Enabled: true},
		2: {ID: 2, Name: "ops-tg", Type: "telegram", Enabled: true},
		3: {ID: 3, Name: "pd", Type: "pagerduty", Enabled: true},
		4: {ID: 4, Name: "muted", Type: "email", Enabled: false},
		5: {ID: 5, Name: "never-paged", Type: "email", Enabled: true},
	}, pages: map[string][]IncidentPage{"inc-1": {
		{ChannelID: 1, Subject: "Incident detected: lock contention"},
		{ChannelID: 2, Subject: "Incident detected: lock contention",
			ProviderMessageID: "4242"},
		{ChannelID: 3, Subject: "Incident detected: lock contention"},
		{ChannelID: 4, Subject: "Incident detected: lock contention"},
	}}}
	d := NewDispatcherWithStore(st, func(string, string, ...any) {})
	senders := map[string]*mockSender{}
	for _, typ := range []string{"slack", "telegram", "pagerduty", "email"} {
		senders[typ] = newMockSender(typ)
		d.RegisterSender(senders[typ])
	}
	return st, d, senders
}

func TestFollowUp_GoesOnlyWhereThePageWent(t *testing.T) {
	st, d, senders := followUpFixture()
	evt := InvestigationConcludedEvent(lockConclusion())
	if err := d.FollowUp(context.Background(), "inc-1", evt); err != nil {
		t.Fatalf("FollowUp: %v", err)
	}
	if senders["slack"].callCount() != 1 || senders["telegram"].callCount() != 1 {
		t.Fatalf("slack %d, telegram %d sends, want 1 each",
			senders["slack"].callCount(), senders["telegram"].callCount())
	}
	if n := senders["pagerduty"].callCount(); n != 0 {
		t.Fatalf("PagerDuty got %d follow-ups (Events v2 has no note)", n)
	}
	if n := senders["email"].callCount(); n != 0 {
		t.Fatalf("a disabled or never-paged channel got %d follow-ups", n)
	}
	slack := senders["slack"].calls[0].Event
	if slack.Subject != "Re: Incident detected: lock contention" || slack.Body != evt.Body ||
		slack.Type != EventInvestigationConcluded || slack.RefID != "inc-1" {
		t.Fatalf("slack follow-up = %+v", slack)
	}
	if _, threaded := slack.Data[DataReplyTo]; threaded {
		t.Fatal("slack follow-up carries a Telegram reply id")
	}
	tg := senders["telegram"].calls[0].Event
	if tg.Data[DataReplyTo] != int64(4242) {
		t.Fatalf("telegram reply_to = %#v, want int64 4242", tg.Data[DataReplyTo])
	}
	if _, mutated := evt.Data[DataReplyTo]; mutated {
		t.Fatal("FollowUp mutated the caller's event data")
	}
	recs := st.recorded()
	if len(recs) != 2 {
		t.Fatalf("%d deliveries recorded, want 2", len(recs))
	}
	for _, r := range recs {
		if r.Status != "sent" || r.Event.RefID != "inc-1" ||
			r.Event.Type != EventInvestigationConcluded {
			t.Fatalf("recorded delivery = %+v", r)
		}
	}
}

func TestFollowUp_NoPageNoFollowUp(t *testing.T) {
	st, d, senders := followUpFixture()
	if err := d.FollowUp(context.Background(), "inc-unpaged",
		InvestigationConcludedEvent(lockConclusion())); err != nil {
		t.Fatalf("FollowUp: %v", err)
	}
	for typ, s := range senders {
		if s.callCount() != 0 {
			t.Fatalf("%s got a follow-up for an incident that was never paged", typ)
		}
	}
	if n := len(st.recorded()); n != 0 {
		t.Fatalf("%d deliveries recorded, want 0", n)
	}
}

func TestFollowUp_InvalidInput(t *testing.T) {
	_, d, _ := followUpFixture()
	err := d.FollowUp(context.Background(), "", Event{Type: EventInvestigationConcluded})
	if err == nil || !strings.Contains(err.Error(), "incident") {
		t.Fatalf("empty incident id err = %v", err)
	}
	plain := NewDispatcherWithStore(&fakeRuleStore{}, func(string, string, ...any) {})
	err = plain.FollowUp(context.Background(), "inc-1", Event{})
	if !errors.Is(err, ErrFollowUpUnsupported) {
		t.Fatalf("store without incident pages err = %v", err)
	}
}

func TestFollowUp_PageLookupErrorPropagates(t *testing.T) {
	st, d, senders := followUpFixture()
	st.pagesErr = errors.New("connection refused")
	err := d.FollowUp(context.Background(), "inc-1", InvestigationConcludedEvent(lockConclusion()))
	if err == nil || !strings.Contains(err.Error(), "inc-1") ||
		!strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v, want the incident and the cause", err)
	}
	if senders["slack"].callCount() != 0 {
		t.Fatal("sent despite the lookup error")
	}
}

func TestFollowUp_SendFailureIsRecordedWithTheRef(t *testing.T) {
	st, d, senders := followUpFixture()
	senders["slack"].failNext = true
	err := d.FollowUp(context.Background(), "inc-1", InvestigationConcludedEvent(lockConclusion()))
	if err == nil || !strings.Contains(err.Error(), "mock send failed") {
		t.Fatalf("err = %v, want the send failure", err)
	}
	if senders["telegram"].callCount() != 1 {
		t.Fatal("a slack failure stopped the telegram follow-up")
	}
	var failed []Delivery
	for _, r := range st.recorded() {
		if r.Status == "error" {
			failed = append(failed, r)
		}
	}
	if len(failed) != 1 || failed[0].ChannelID != 1 || failed[0].Event.RefID != "inc-1" ||
		!strings.Contains(failed[0].Error, "mock send failed") {
		t.Fatalf("failed deliveries = %+v", failed)
	}
}

func TestFollowUp_ConcurrentCallsDoNotShareData(t *testing.T) {
	st, d, _ := followUpFixture()
	evt := InvestigationConcludedEvent(lockConclusion())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = d.FollowUp(context.Background(), "inc-1", evt)
		}()
	}
	wg.Wait()
	if n := len(st.recorded()); n != 16 {
		t.Fatalf("%d deliveries, want 16", n)
	}
	if _, mutated := evt.Data[DataReplyTo]; mutated {
		t.Fatal("concurrent follow-ups mutated the shared event")
	}
}

// idSender returns a provider message id for each send.
type idSender struct {
	mockSender
	id string
}

func (s *idSender) SendMessageID(ctx context.Context, ch Channel, evt Event) (string, error) {
	return s.id, s.Send(ctx, ch, evt)
}

func TestDeliver_RecordsTheProviderMessageID(t *testing.T) {
	st := &pageStore{}
	d := NewDispatcherWithStore(st, func(string, string, ...any) {})
	s := &idSender{mockSender: mockSender{typ: "telegram"}, id: "777"}
	d.RegisterSender(s)
	ch := Channel{ID: 2, Name: "tg", Type: "telegram", Enabled: true}
	if err := d.SendDirect(context.Background(), ch,
		IncidentDetectedEvent(IncidentInfo{ID: "inc-5", Severity: "critical"})); err != nil {
		t.Fatalf("send: %v", err)
	}
	recs := st.recorded()
	if len(recs) != 1 || recs[0].ProviderMessageID != "777" || recs[0].Event.RefID != "inc-5" ||
		recs[0].Status != "sent" {
		t.Fatalf("recorded = %+v", recs)
	}
	s.failNext = true
	_ = d.SendDirect(context.Background(), ch, IncidentDetectedEvent(IncidentInfo{ID: "inc-6"}))
	if recs = st.recorded(); recs[1].Status != "error" || recs[1].ProviderMessageID != "" {
		t.Fatalf("a failed send recorded message id %q (%s)", recs[1].ProviderMessageID,
			recs[1].Status)
	}
}

func telegramReplyServer(t *testing.T, status int, reply string) (*httptest.Server,
	*[]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		mu.Lock()
		bodies = append(bodies, m)
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

func TestTelegram_SendMessageIDParsesTheResult(t *testing.T) {
	srv, _ := telegramReplyServer(t, http.StatusOK,
		`{"ok":true,"result":{"message_id":9876,"chat":{"id":-1001}}}`)
	ch := Channel{Name: "tg", Type: "telegram", Config: map[string]string{
		"bot_token": "123:tok", "chat_id": "-1001"}}
	id, err := telegramSender(srv.URL).SendMessageID(context.Background(), ch,
		Event{Subject: "s", Body: "b"})
	if err != nil || id != "9876" {
		t.Fatalf("id = %q, err = %v, want 9876", id, err)
	}
}

func TestTelegram_SendMessageIDToleratesAnUnreadableReply(t *testing.T) {
	ch := Channel{Name: "tg", Type: "telegram", Config: map[string]string{
		"bot_token": "123:tok", "chat_id": "-1001"}}
	for _, reply := range []string{"", "not json", `{"ok":true,"result":{}}`,
		`{"ok":true,"result":{"message_id":-3}}`} {
		srv, _ := telegramReplyServer(t, http.StatusOK, reply)
		id, err := telegramSender(srv.URL).SendMessageID(context.Background(), ch, Event{})
		if err != nil || id != "" {
			t.Fatalf("reply %q: id %q err %v, want a sent message without an id",
				reply, id, err)
		}
	}
	srv, _ := telegramReplyServer(t, http.StatusBadGateway, `{"ok":false}`)
	id, err := telegramSender(srv.URL).SendMessageID(context.Background(), ch, Event{})
	if err == nil || id != "" || !strings.Contains(err.Error(), "502") ||
		strings.Contains(err.Error(), "123:tok") {
		t.Fatalf("5xx: id %q err %v", id, err)
	}
}
