package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// sre-08 composed: an incident paged to Slack and Telegram, then
// investigated by the real coordinator, gets one diagnosis follow-up per
// paged channel; the Telegram one replies to the page, and a planted
// application name (in the trigger subject and an idle session the probes
// see) reaches neither a delivered body nor sage.notification_log.

const plantedFollowUpApp = "billing-planted-app password=planted-app-secret"

// threadSender is a Telegram-typed sender that returns message ids.
type threadSender struct {
	mu     sync.Mutex
	events []notify.Event
	next   int
}

func (s *threadSender) Type() string { return "telegram" }

func (s *threadSender) Send(ctx context.Context, ch notify.Channel, evt notify.Event) error {
	_, err := s.SendMessageID(ctx, ch, evt)
	return err
}

func (s *threadSender) SendMessageID(_ context.Context, _ notify.Channel,
	evt notify.Event) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, evt)
	s.next++
	return fmt.Sprint(500 + s.next), nil
}

func (s *threadSender) sent() []notify.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]notify.Event(nil), s.events...)
}

func TestConclusionSink_NilDispatcherIsNoSink(t *testing.T) {
	if s := newConclusionSink(nil); s != nil {
		t.Fatalf("a nil dispatcher built sink %#v", s)
	}
}

func TestConclusionSink_MapsTheSignal(t *testing.T) {
	st := &followUpPages{pages: []notify.IncidentPage{{ChannelID: 7,
		Subject: "Incident detected: lock contention", ProviderMessageID: "42"}}}
	d := notify.NewDispatcherWithStore(st, func(string, string, ...any) {})
	tg := &threadSender{}
	d.RegisterSender(tg)
	sig := sre.ConclusionSignal{InvestigationID: "inv-1", IncidentID: "inc-1",
		CaseID: "case-1", Database: "orders", Family: "lock_blocking",
		State: sre.StateConcluded, RootLabel: "idle-in-transaction holder",
		Reason: "pid 7 idle", Severity: "critical"}
	if err := newConclusionSink(d).DeliverConclusion(context.Background(), sig); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	got := tg.sent()
	want := "Diagnosis for incident inc-1 on orders: lock_blocking - " +
		"idle-in-transaction holder: pid 7 idle. Case case-1."
	if len(got) != 1 || got[0].Body != want || got[0].Severity != "critical" ||
		got[0].Data[notify.DataReplyTo] != int64(42) || st.incident != "inc-1" {
		t.Fatalf("delivered %+v (looked up %q)", got, st.incident)
	}
}

// followUpPages is a RuleStore with one channel and fixed incident pages.
type followUpPages struct {
	pages    []notify.IncidentPage
	incident string
}

func (s *followUpPages) MatchingRules(context.Context, string) ([]notify.Rule, error) {
	return nil, nil
}

func (s *followUpPages) Channel(_ context.Context, id int) (*notify.Channel, error) {
	return &notify.Channel{ID: id, Name: "tg", Type: "telegram", Enabled: true}, nil
}

func (s *followUpPages) LogDelivery(context.Context, int, notify.Event, string,
	string) error {
	return nil
}

func (s *followUpPages) IncidentPages(_ context.Context, id string) ([]notify.IncidentPage,
	error) {
	s.incident = id
	return s.pages, nil
}

func followUpChannel(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name,
	typ string, cfgJSON string) int {
	t.Helper()
	var id int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.notification_channels
		(name, type, config) VALUES ($1, $2, $3::jsonb) RETURNING id`,
		name, typ, cfgJSON).Scan(&id); err != nil {
		t.Fatalf("insert channel %s: %v", name, err)
	}
	t.Cleanup(func() {
		for _, q := range []string{"DELETE FROM sage.notification_rules WHERE channel_id = $1",
			"DELETE FROM sage.notification_log WHERE channel_id = $1",
			"DELETE FROM sage.notification_channels WHERE id = $1"} {
			_, _ = pool.Exec(context.Background(), q, id)
		}
	})
	if _, err := pool.Exec(ctx, `INSERT INTO sage.notification_rules
		(channel_id, event, min_severity) VALUES ($1, 'incident_detected', 'info')`,
		id); err != nil {
		t.Fatalf("insert rule: %v", err)
	}
	return id
}

// plantIdleSession holds an idle transaction under a planted application
// name, so the lock probes collect it as evidence.
func plantIdleSession(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cc.RuntimeParams["application_name"] = plantedFollowUpApp
	conn, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		t.Fatalf("planted session: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	for _, q := range []string{"BEGIN", "SELECT count(*) FROM pg_class"} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("planted %s: %v", q, err)
		}
	}
}

// insertComposedIncident records an open lock incident of fu_db.
func insertComposedIncident(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var inc string
	if err := pool.QueryRow(ctx, `INSERT INTO sage.incidents
		(severity, root_cause, signal_ids, source, database_name)
		VALUES ('critical', 'lock contention', ARRAY['lock_contention'], 'deterministic',
		        'fu_db') RETURNING id::text`).Scan(&inc); err != nil {
		t.Fatalf("insert incident: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.incidents
			WHERE id = $1::uuid`, inc)
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.sre_incident_followup
			WHERE incident_id = $1`, inc)
	})
	return inc
}

func TestSREConclusionFollowUp_Composed(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool := openComposedPool(t, ctx, dsn)
	hook := newHookSink(t)
	tag := fmt.Sprintf("sre08-%d", time.Now().UnixNano())
	slackID := followUpChannel(t, ctx, pool, tag+"-slack", "slack",
		`{"webhook_url":"`+hook.srv.URL+`"}`)
	tgID := followUpChannel(t, ctx, pool, tag+"-tg", "telegram",
		`{"bot_token":"1:x","chat_id":"-1"}`)
	d := notify.NewDispatcher(pool, func(string, string, ...any) {})
	d.RegisterSender(notify.NewSlackSenderWithPolicy(notify.TargetPolicy{AllowPrivate: true}))
	tg := &threadSender{}
	d.RegisterSender(tg)
	inc := insertComposedIncident(t, ctx, pool)
	if err := d.Dispatch(ctx, notify.IncidentDetectedEvent(notify.IncidentInfo{ID: inc,
		Database: "fu_db", Severity: "critical", RootCause: "lock contention"})); err != nil {
		t.Fatalf("page: %v", err)
	}
	plantIdleSession(t, ctx, dsn)
	svc := followUpInvestigator(t, pool, d)
	inv, _, err := svc.Coordinator().Start(ctx, sre.Trigger{CaseID: "case:" + tag,
		IncidentID: inc, Kind: sre.TriggerLock, Subject: "incident " + plantedFollowUpApp,
		IdempotencyKey: "incident:" + inc})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := svc.Coordinator().Investigate(ctx, inv.ID); err != nil {
		t.Fatalf("investigate: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.sre_investigations
		SET concluded_at = clock_timestamp() - interval '2 minutes' WHERE id = $1::uuid
		AND state IN ('concluded', 'inconclusive')`, string(inv.ID)); err != nil {
		t.Fatalf("age: %v", err)
	}
	runLoopOnce(t, ctx, svc.Coordinator(), pool, inc)
	checkComposedFollowUp(t, ctx, pool, hook, tg, inc, slackID, tgID)
}

func followUpInvestigator(t *testing.T, pool *pgxpool.Pool, d *notify.Dispatcher) *sre.Service {
	t.Helper()
	settings := config.DefaultConfig().SRE
	svc, err := newSREInvestigator(sreInvestigatorDeps{control: pool, monitored: pool,
		runner: probes.NewRunner(pool, probes.Catalog(), probes.NewLimiter(1)),
		name:   "fu_db", runtimeKey: fmt.Sprintf("fu:%d", time.Now().UnixNano()),
		settings: settings, logFn: func(string, string, ...any) {},
		followUps: newConclusionSink(d)})
	if err != nil {
		t.Fatalf("investigator: %v", err)
	}
	return svc
}

// runLoopOnce runs the coordinator loop until the incident's follow-up
// claim is settled (delivered or failed), then stops it.
func runLoopOnce(t *testing.T, ctx context.Context, c *sre.Coordinator, pool *pgxpool.Pool,
	inc string) {
	t.Helper()
	loopCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); c.Run(loopCtx) }()
	defer func() { stop(); <-done }()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		var delivered bool
		err := pool.QueryRow(ctx, `SELECT delivered_at IS NOT NULL
			FROM sage.sre_incident_followup WHERE incident_id = $1`, inc).Scan(&delivered)
		if err == nil && delivered {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no delivered follow-up claim for incident %s", inc)
}

func checkComposedFollowUp(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	hook *hookSink, tg *threadSender, inc string, slackID, tgID int) {
	t.Helper()
	tgEvents := tg.sent()
	if len(tgEvents) != 2 || tgEvents[1].Type != notify.EventInvestigationConcluded ||
		tgEvents[1].Data[notify.DataReplyTo] != int64(501) {
		t.Fatalf("telegram events = %+v, want the page then a reply to 501", tgEvents)
	}
	if !strings.HasPrefix(tgEvents[1].Body, "Diagnosis for incident "+inc+" on fu_db: ") {
		t.Fatalf("telegram body = %q", tgEvents[1].Body)
	}
	if n := len(hook.matching("Diagnosis for incident " + inc)); n != 1 {
		t.Fatalf("slack got %d follow-ups, want 1", n)
	}
	var planted int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.sre_investigations
		WHERE source_incident_id = $1 AND subject LIKE '%billing-planted-app%'`, inc).
		Scan(&planted); err != nil || planted != 1 {
		t.Fatalf("the investigation subject does not hold the planted name (%d, %v)",
			planted, err)
	}
	for _, b := range append(hook.matching(inc), tgEvents[1].Body) {
		if strings.Contains(b, "billing-planted-app") || strings.Contains(b, "planted-app-secret") {
			t.Fatalf("CHECK-36: a delivered body leaks the planted session: %s", b)
		}
	}
	var rows, leaks int
	if err := pool.QueryRow(ctx, `SELECT count(*),
		    count(*) FILTER (WHERE body LIKE '%planted-app%')
		FROM sage.notification_log WHERE ref_id = $1 AND event = 'investigation_concluded'
		  AND status = 'sent' AND channel_id = ANY($2::int[])`, inc,
		[]int{slackID, tgID}).Scan(&rows, &leaks); err != nil || rows != 2 || leaks != 0 {
		t.Fatalf("follow-up log rows %d, leaking %d (%v); want 2 and 0", rows, leaks, err)
	}
	var dupes int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT incident_id
		FROM sage.sre_incident_followup GROUP BY 1 HAVING count(*) > 1) d`).
		Scan(&dupes); err != nil || dupes != 0 {
		t.Fatalf("CHECK-34: %d duplicate claims (%v)", dupes, err)
	}
}
