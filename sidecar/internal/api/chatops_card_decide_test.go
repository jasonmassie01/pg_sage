package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// policy-04: an approval outside the maintenance window is scheduled for
// the next opening, from chat and from the API. The chat readiness check
// runs before the card token is consumed, so a card refused for any other
// reason stays usable. Real PostgreSQL; the standing gate's clock is fixed
// a week ahead, so the planned run is never due during the test.

// windowClock is a Wednesday 14:00 UTC at least a week ahead.
func windowClock() func() time.Time {
	day := time.Now().UTC().Add(7 * 24 * time.Hour)
	for day.Weekday() != time.Wednesday {
		day = day.Add(24 * time.Hour)
	}
	var at atomic.Int64
	at.Store(time.Date(day.Year(), day.Month(), day.Day(), 14, 0, 0, 0,
		time.UTC).UnixNano())
	return func() time.Time { return time.Unix(0, at.Load()).UTC() }
}

// staffedChatPolicy is the staffed profile in UTC, optionally narrowed to
// ANALYZE only (a refusal that is not the window).
func staffedChatPolicy(narrowed bool) policy.Document {
	doc := policy.StaffedProfile()
	doc.MaintenanceWindows = []string{"weekdays 01:00-05:00 America/Chicago"}
	if narrowed {
		doc.AllowedChangeClasses = []policy.ChangeClass{policy.ChangeAnalyze}
	}
	return doc
}

// useModerateItem replaces the fixture's ANALYZE with a pending CREATE
// INDEX CONCURRENTLY (a moderate action the window binds) under the
// staffed policy, outside its window.
func (cf *chatCardFixture) useModerateItem(t *testing.T, narrowed bool) {
	t.Helper()
	ctx := context.Background()
	cf.sql = fmt.Sprintf("CREATE INDEX CONCURRENTLY %s_id ON public.%s (id)", cf.table,
		cf.table)
	if err := cf.pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommendation, recommended_sql,
		status) VALUES ('missing_index', 'warning', 'table', $1, 'Missing index on ' || $1,
		'{}', 'Create it', $2, 'open') RETURNING id`, "public."+cf.table,
		cf.sql).Scan(&cf.findingID); err != nil {
		t.Fatal(err)
	}
	id, err := store.NewActionStore(cf.pool).ProposeWithMetadata(ctx, nil, cf.findingID,
		cf.sql, "", "moderate", store.ActionProposalMetadata{
			ActionType: "create_index_concurrently", PolicyDecision: "queue_approval"})
	if err != nil {
		t.Fatal(err)
	}
	cf.queueID = id
	cf.exec.EnableStandingPolicyDocument(staffedChatPolicy(narrowed), windowClock())
}

func TestChatApproveOutsideTheWindowSchedules(t *testing.T) {
	cf := newChatCardFixture(t)
	cf.useModerateItem(t, false)
	token := cf.issue(t, cf.slackCh)
	code, body := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackCardApproveAction, token,
		testSigningSecret)
	if code != http.StatusOK || body["status"] != "scheduled" || body["executed"] != false {
		t.Fatalf("approve outside the window: %d %v, want 200 scheduled", code, body)
	}
	var status string
	var scheduledBy *int
	var runAfter *time.Time
	if err := cf.pool.QueryRow(context.Background(), `SELECT status, scheduled_by,
		run_after FROM sage.action_queue WHERE id = $1`, cf.queueID).Scan(&status,
		&scheduledBy, &runAfter); err != nil {
		t.Fatal(err)
	}
	if status != "scheduled" || scheduledBy == nil || *scheduledBy != cf.operator ||
		runAfter == nil || cf.executions(t) != 0 {
		t.Fatalf("queue %q by %v at %v, executions %d", status, scheduledBy, runAfter,
			cf.executions(t))
	}
	reply := cf.replies.last()
	if !strings.Contains(reply, "Scheduled for the window opening Thu ") ||
		!strings.Contains(reply, "01:00 America/Chicago") ||
		!strings.Contains(reply, "cancel in pg_sage") {
		t.Fatalf("reply = %q, want the next opening in the window's zone", reply)
	}
	d, err := cf.cards.Lookup(context.Background(), token)
	if err != nil || d.UsedAt == nil {
		t.Fatalf("a scheduled card must consume its token: %+v, %v", d, err)
	}
}

func TestChatApproveRefusedForAnotherReasonKeepsTheToken(t *testing.T) {
	cf := newChatCardFixture(t)
	cf.useModerateItem(t, true)
	token := cf.issue(t, cf.slackCh)
	code, body := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackCardApproveAction, token,
		testSigningSecret)
	if code != http.StatusConflict || !strings.Contains(fmt.Sprint(body["error"]),
		"change class not allowed") {
		t.Fatalf("approve a class refusal: %d %v, want 409 naming the refusal", code, body)
	}
	d, err := cf.cards.Lookup(context.Background(), token)
	if err != nil || d.UsedAt != nil {
		t.Fatalf("a refused card must keep its token: %+v, %v", d, err)
	}
	if cf.executions(t) != 0 {
		t.Fatalf("executions = %d", cf.executions(t))
	}
	if reply := cf.replies.last(); !strings.Contains(reply, "Not approved") {
		t.Fatalf("reply = %q, want the refusal", reply)
	}
}

func TestChatSnoozeOutsideTheWindowStillSnoozes(t *testing.T) {
	// The readiness check binds Approve only: a Snooze outside the window
	// defers the card as before.
	cf := newChatCardFixture(t)
	cf.useModerateItem(t, false)
	token := cf.issue(t, cf.slackCh)
	code, body := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackCardSnoozeAction, token,
		testSigningSecret)
	if code != http.StatusOK || body["status"] != "snoozed" {
		t.Fatalf("snooze: %d %v", code, body)
	}
}

func TestScheduleAndUnscheduleRoutes(t *testing.T) {
	cf := newChatCardFixture(t)
	cf.useModerateItem(t, false)
	h := cf.router(t, operatorUser())
	path := func(verb string) string {
		return fmt.Sprintf("/api/v1/actions/%d/%s?database=orders", cf.queueID, verb)
	}
	code, body := cardCall(t, h, http.MethodPost, path("schedule"), nil)
	if code != http.StatusOK || body["status"] != "scheduled" || body["run_after"] == nil ||
		body["window_closes_at"] == nil {
		t.Fatalf("schedule: %d %v", code, body)
	}
	if status, _ := cf.status(t); status != "scheduled" {
		t.Fatalf("status after schedule = %q", status)
	}
	code, body = cardCall(t, h, http.MethodPost, path("schedule"), nil)
	if code != http.StatusConflict {
		t.Fatalf("schedule twice: %d %v, want 409", code, body)
	}
	code, body = cardCall(t, h, http.MethodPost, path("unschedule"), nil)
	if code != http.StatusOK || body["status"] != "pending" {
		t.Fatalf("unschedule: %d %v", code, body)
	}
	if status, reason := cf.status(t); status != "pending" ||
		!strings.Contains(reason, "cancelled") {
		t.Fatalf("after unschedule: %q %q", status, reason)
	}
	code, body = cardCall(t, h, http.MethodPost, path("unschedule"), nil)
	if code != http.StatusConflict {
		t.Fatalf("unschedule a pending row: %d %v, want 409", code, body)
	}
}

func TestScheduleRouteRefusesANonWindowRefusal(t *testing.T) {
	cf := newChatCardFixture(t)
	cf.useModerateItem(t, true)
	code, body := cardCall(t, cf.router(t, operatorUser()), http.MethodPost,
		fmt.Sprintf("/api/v1/actions/%d/schedule?database=orders", cf.queueID), nil)
	if code != http.StatusConflict || !strings.Contains(fmt.Sprint(body["error"]),
		"change class not allowed") {
		t.Fatalf("schedule a class refusal: %d %v, want 409", code, body)
	}
	if status, _ := cf.status(t); status != "pending" {
		t.Fatalf("a refused schedule changed the row to %q", status)
	}
}

func TestScheduleRoutesNeedAnOperator(t *testing.T) {
	cf := newChatCardFixture(t)
	cf.useModerateItem(t, false)
	for _, verb := range []string{"schedule", "unschedule"} {
		code, _ := cardCall(t, cf.router(t, viewerUser()), http.MethodPost,
			fmt.Sprintf("/api/v1/actions/%d/%s?database=orders", cf.queueID, verb), nil)
		if code != http.StatusForbidden {
			t.Fatalf("viewer %s: %d, want 403", verb, code)
		}
	}
	code, _ := cardCall(t, cf.router(t, operatorUser()), http.MethodPost,
		"/api/v1/actions/abc/schedule?database=orders", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("schedule a non-numeric id: %d, want 400", code)
	}
}

func TestApproveRouteOutsideTheWindowSchedules(t *testing.T) {
	cf := newChatCardFixture(t)
	cf.useModerateItem(t, false)
	code, body := cardCall(t, cf.router(t, operatorUser()), http.MethodPost,
		fmt.Sprintf("/api/v1/actions/%d/approve?database=orders", cf.queueID), nil)
	if code != http.StatusOK || body["status"] != "scheduled" || body["executed"] != false {
		t.Fatalf("approve outside the window: %d %v, want 200 scheduled", code, body)
	}
	if status, _ := cf.status(t); status != "scheduled" || cf.executions(t) != 0 {
		t.Fatalf("status %q, executions %d", status, cf.executions(t))
	}
}

func TestPendingListShowsScheduledApprovals(t *testing.T) {
	cf := newChatCardFixture(t)
	cf.useModerateItem(t, false)
	h := cf.router(t, operatorUser())
	code, body := cardCall(t, h, http.MethodPost,
		fmt.Sprintf("/api/v1/actions/%d/schedule?database=orders", cf.queueID), nil)
	if code != http.StatusOK {
		t.Fatalf("schedule: %d %v", code, body)
	}
	code, body = cardCall(t, h, http.MethodGet, "/api/v1/actions/pending?database=orders", nil)
	if code != http.StatusOK {
		t.Fatalf("pending: %d %v", code, body)
	}
	scheduled, _ := body["scheduled"].([]any)
	var found map[string]any
	for _, row := range scheduled {
		if m, _ := row.(map[string]any); fmt.Sprint(m["id"]) == fmt.Sprint(cf.queueID) {
			found = m
		}
	}
	if found == nil || found["status"] != "scheduled" || found["run_after"] == nil ||
		found["database_name"] != "orders" || fmt.Sprint(found["scheduled_by"]) != "2" {
		t.Fatalf("scheduled = %v, want the scheduled row with its run_after", scheduled)
	}
	pending, _ := body["pending"].([]any)
	for _, row := range pending {
		if m, _ := row.(map[string]any); fmt.Sprint(m["id"]) == fmt.Sprint(cf.queueID) {
			t.Fatalf("the scheduled row is also listed as pending: %v", m)
		}
	}
}
