package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// sre-08 on real PostgreSQL: incident pages record their incident and
// Telegram message id in sage.notification_log, and the diagnosis
// follow-up reaches exactly the channels whose page was sent.

// hookServer records request bodies and answers with status and reply.
type hookServer struct {
	mu     sync.Mutex
	status int
	reply  string
	bodies []map[string]any
}

func (h *hookServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		h.mu.Lock()
		h.bodies = append(h.bodies, m)
		status, reply := h.status, h.reply
		h.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (h *hookServer) set(status int, reply string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status, h.reply = status, reply
}

func (h *hookServer) requests() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]any(nil), h.bodies...)
}

type followUpDB struct {
	pool          *pgxpool.Pool
	d             *Dispatcher
	slack, tg     *hookServer
	slackID, tgID int
}

func newFollowUpDB(t *testing.T) *followUpDB {
	t.Helper()
	pool := connectTestDB(t)
	f := &followUpDB{pool: pool, slack: &hookServer{status: 200, reply: "ok"},
		tg: &hookServer{status: 200,
			reply: `{"ok":true,"result":{"message_id":777,"chat":{"id":-1001}}}`}}
	slackSrv, tgSrv := f.slack.start(t), f.tg.start(t)
	tag := fmt.Sprintf("sre08_test_%d", time.Now().UnixNano())
	f.slackID = insertTestChannel(t, pool, tag+"_slack", "slack",
		map[string]string{"webhook_url": slackSrv.URL}, true)
	f.tgID = insertTestChannel(t, pool, tag+"_tg", "telegram",
		map[string]string{"bot_token": "123:tok", "chat_id": "-1001"}, true)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx, `DELETE FROM sage.notification_log
			WHERE channel_id = ANY($1::int[])`, []int{f.slackID, f.tgID})
		_, _ = pool.Exec(ctx, `DELETE FROM sage.notification_channels
			WHERE id = ANY($1::int[])`, []int{f.slackID, f.tgID})
	})
	for _, ch := range []int{f.slackID, f.tgID} {
		insertTestRule(t, pool, ch, "incident_detected", "info", true)
	}
	f.d = NewDispatcher(pool, func(string, string, ...any) {})
	f.d.RegisterSender(NewSlackSenderWithPolicy(TargetPolicy{AllowPrivate: true}))
	f.d.RegisterSender(telegramSender(tgSrv.URL))
	return f
}

type logRow struct {
	channel             int
	event, status, body string
	ref, msgID          string
}

func (f *followUpDB) logOf(t *testing.T, incident string) []logRow {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT channel_id, event, status,
		    COALESCE(body, ''), COALESCE(ref_id, ''), COALESCE(provider_message_id, '')
		FROM sage.notification_log
		WHERE ref_id = $1 AND channel_id = ANY($2::int[]) ORDER BY id`,
		incident, []int{f.slackID, f.tgID})
	if err != nil {
		t.Fatalf("read notification_log: %v", err)
	}
	defer rows.Close()
	var out []logRow
	for rows.Next() {
		var r logRow
		if err := rows.Scan(&r.channel, &r.event, &r.status, &r.body, &r.ref,
			&r.msgID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func (f *followUpDB) page(t *testing.T, incident string) {
	t.Helper()
	if err := f.d.Dispatch(context.Background(), IncidentDetectedEvent(IncidentInfo{
		ID: incident, Database: "orders", Severity: "critical",
		RootCause: "lock contention"})); err != nil {
		t.Fatalf("dispatch page: %v", err)
	}
}

func conclusionFor(incident string) Event {
	info := lockConclusion()
	info.IncidentID = incident
	return InvestigationConcludedEvent(info)
}

func TestFollowUpDB_ThreadsUnderThePage(t *testing.T) {
	f := newFollowUpDB(t)
	inc := fmt.Sprintf("inc-%d", time.Now().UnixNano())
	f.page(t, inc)
	pages := f.logOf(t, inc)
	if len(pages) != 2 {
		t.Fatalf("%d page rows with ref_id %s, want 2: %+v", len(pages), inc, pages)
	}
	for _, p := range pages {
		want := ""
		if p.channel == f.tgID {
			want = "777"
		}
		if p.event != "incident_detected" || p.status != "sent" || p.msgID != want {
			t.Fatalf("page row = %+v, want sent with message id %q", p, want)
		}
	}
	if err := f.d.FollowUp(context.Background(), inc, conclusionFor(inc)); err != nil {
		t.Fatalf("FollowUp: %v", err)
	}
	tgReqs, slackReqs := f.tg.requests(), f.slack.requests()
	if len(tgReqs) != 2 || len(slackReqs) != 2 {
		t.Fatalf("telegram %d, slack %d requests, want 2 each", len(tgReqs), len(slackReqs))
	}
	reply := tgReqs[1]
	if reply["reply_to_message_id"] != float64(777) ||
		!strings.HasPrefix(fmt.Sprint(reply["text"]), "Re: Incident detected: lock contention") {
		t.Fatalf("telegram follow-up = %v, want a reply to 777 titled Re:", reply)
	}
	if !strings.Contains(fmt.Sprint(slackReqs[1]), "Re: Incident detected: lock contention") {
		t.Fatalf("slack follow-up = %v, want the Re: subject", slackReqs[1])
	}
	var follow []logRow
	for _, r := range f.logOf(t, inc) {
		if r.event == EventInvestigationConcluded {
			follow = append(follow, r)
		}
	}
	if len(follow) != 2 || follow[0].status != "sent" || follow[1].status != "sent" ||
		!strings.Contains(follow[0].body, "Diagnosis for incident "+inc) {
		t.Fatalf("follow-up rows = %+v", follow)
	}
}

func TestFollowUpDB_UnsentPageGetsNoFollowUp(t *testing.T) {
	f := newFollowUpDB(t)
	f.slack.set(http.StatusInternalServerError, "down")
	f.tg.set(http.StatusBadGateway, `{"ok":false}`)
	inc := fmt.Sprintf("inc-%d", time.Now().UnixNano())
	f.page(t, inc)
	for _, p := range f.logOf(t, inc) {
		if p.status != "error" {
			t.Fatalf("page row = %+v, want error", p)
		}
	}
	f.slack.set(http.StatusOK, "ok")
	f.tg.set(http.StatusOK, `{"ok":true,"result":{"message_id":1}}`)
	if err := f.d.FollowUp(context.Background(), inc, conclusionFor(inc)); err != nil {
		t.Fatalf("FollowUp: %v", err)
	}
	if n := len(f.slack.requests()) + len(f.tg.requests()); n != 2 {
		t.Fatalf("%d requests, want only the 2 failed pages", n)
	}
	if never := fmt.Sprintf("inc-never-%d", time.Now().UnixNano()); f.d.FollowUp(
		context.Background(), never, conclusionFor(never)) != nil ||
		len(f.slack.requests()) != 1 {
		t.Fatal("a never-paged incident got a follow-up")
	}
}

func TestFollowUpDB_ChannelErrorIsLoggedWithTheRef(t *testing.T) {
	f := newFollowUpDB(t)
	inc := fmt.Sprintf("inc-%d", time.Now().UnixNano())
	f.page(t, inc)
	f.slack.set(http.StatusServiceUnavailable, "busy")
	err := f.d.FollowUp(context.Background(), inc, conclusionFor(inc))
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want the slack 503", err)
	}
	var slackErr, tgSent int
	for _, r := range f.logOf(t, inc) {
		switch {
		case r.event != EventInvestigationConcluded:
		case r.channel == f.slackID && r.status == "error" && r.ref == inc:
			slackErr++
		case r.channel == f.tgID && r.status == "sent":
			tgSent++
		}
	}
	if slackErr != 1 || tgSent != 1 {
		t.Fatalf("slack error rows %d, telegram sent rows %d, want 1 and 1", slackErr, tgSent)
	}
	var withErr int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.notification_log
		WHERE ref_id = $1 AND status = 'error' AND error LIKE '%503%'`, inc).
		Scan(&withErr); err != nil || withErr != 1 {
		t.Fatalf("error rows naming the 503: %d (%v)", withErr, err)
	}
}

func TestFollowUpDB_EscalationAloneCountsAsAPage(t *testing.T) {
	f := newFollowUpDB(t)
	insertTestRule(t, f.pool, f.slackID, "incident_escalated", "info", true)
	inc := fmt.Sprintf("inc-%d", time.Now().UnixNano())
	if err := f.d.Dispatch(context.Background(), IncidentEscalatedEvent(IncidentInfo{
		ID: inc, Database: "orders", Severity: "critical", RootCause: "lock"})); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	if err := f.d.FollowUp(context.Background(), inc, conclusionFor(inc)); err != nil {
		t.Fatalf("FollowUp: %v", err)
	}
	if n, m := len(f.slack.requests()), len(f.tg.requests()); n != 2 || m != 0 {
		t.Fatalf("slack %d telegram %d requests, want 2 and 0", n, m)
	}
}
