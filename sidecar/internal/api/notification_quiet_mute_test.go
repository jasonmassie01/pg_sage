package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/store"
)

// ux-09b: the channel form's quiet hours, the mute dialog and the weekly
// line. The handlers run over the notify MemoryStore and small fakes; the
// SQL behind them is covered by the notify and store DB tests.

var muteNow = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func fixedNow() time.Time { return muteNow }

var muteAdmin = &auth.User{ID: 4, Email: "admin@example.com", Role: "admin"}

func decodeMap(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return out
}

func TestCreateMuteHandler_StoresTheMuteWithTheActor(t *testing.T) {
	ms := notify.NewMemoryStore()
	h := createMuteHandler(ms, fixedNow)
	w := doRequestWithUser(h, "POST", "/api/v1/notifications/mutes",
		`{"database":"orders","class":"ask","until":"2026-11-19T12:00:00Z","reason":"noisy"}`,
		muteAdmin)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d %s, want 201", w.Code, w.Body.String())
	}
	got := decodeMap(t, w)
	if got["database"] != "orders" || got["class"] != "ask" ||
		got["actor"] != "admin@example.com" || got["reason"] != "noisy" {
		t.Fatalf("response %v", got)
	}
	muted, err := ms.ActiveMute(context.Background(), "orders", notify.ClassAsk, muteNow)
	if err != nil || !muted {
		t.Fatalf("the mute was not stored: %v, %v", muted, err)
	}
}

func TestCreateMuteHandler_RefusesBadMutes(t *testing.T) {
	cases := []struct{ body, want string }{
		{`not json`, "invalid request body"},
		{`{"database":"orders"}`, "until"},
		{`{"database":"orders","until":"tomorrow"}`, "until"},
		{`{"database":"orders","until":"2027-03-01T12:00:00Z"}`, "90 days"},
		{`{"database":"orders","until":"2026-11-18T11:00:00Z"}`, "future"},
		{`{"database":"","until":"2026-11-19T12:00:00Z"}`, "database"},
		{`{"database":"orders","class":"pager","until":"2026-11-19T12:00:00Z"}`, "pager"},
	}
	for _, c := range cases {
		ms := notify.NewMemoryStore()
		w := doRequestWithUser(createMuteHandler(ms, fixedNow), "POST",
			"/api/v1/notifications/mutes", c.body, muteAdmin)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("%s: %d %s, want 400 naming %q", c.body, w.Code, w.Body.String(), c.want)
		}
		if all, _ := ms.ListMutes(context.Background(), time.Time{}); len(all) != 0 {
			t.Errorf("%s: a refused mute was stored: %+v", c.body, all)
		}
	}
	w := doRequest(createMuteHandler(notify.NewMemoryStore(), fixedNow), "POST",
		"/api/v1/notifications/mutes", `{"database":"orders","until":"2026-11-19T12:00:00Z"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no user: status %d, want 401", w.Code)
	}
}

func TestMuteHandlers_ListThenUnmute(t *testing.T) {
	ms := notify.NewMemoryStore()
	if _, err := ms.PutMute(context.Background(), notify.Mute{Database: "orders",
		Until: muteNow.Add(time.Hour), Actor: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.PutMute(context.Background(), notify.Mute{Database: "old",
		Until: muteNow.Add(-time.Hour), Actor: "a"}); err != nil {
		t.Fatal(err)
	}
	w := doRequest(listMutesHandler(ms, fixedNow), "GET", "/api/v1/notifications/mutes", "")
	var list struct {
		Mutes []notify.Mute `json:"mutes"`
	}
	if err := json.NewDecoder(w.Body).Decode(&list); err != nil || w.Code != 200 ||
		len(list.Mutes) != 1 || list.Mutes[0].Database != "orders" {
		t.Fatalf("list %d %+v, %v; want only the active orders mute", w.Code, list, err)
	}
	del := deleteMuteHandler(ms)
	w = doRequest(del, "DELETE", "/api/v1/notifications/mutes?database=orders", "")
	if w.Code != http.StatusOK {
		t.Fatalf("unmute status %d %s", w.Code, w.Body.String())
	}
	w = doRequest(del, "DELETE", "/api/v1/notifications/mutes?database=orders", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("a second unmute status %d, want 404", w.Code)
	}
	for _, q := range []string{"", "?database=", "?database=orders&class=pager"} {
		w = doRequest(del, "DELETE", "/api/v1/notifications/mutes"+q, "")
		if w.Code != http.StatusBadRequest {
			t.Errorf("unmute %q status %d, want 400", q, w.Code)
		}
	}
}

// failingMuteStore fails every call.
type failingMuteStore struct{}

var errMuteTable = errors.New("relation sage.notification_mutes does not exist")

func (failingMuteStore) PutMute(context.Context, notify.Mute) (notify.Mute, error) {
	return notify.Mute{}, errMuteTable
}
func (failingMuteStore) DeleteMute(context.Context, string, string) (bool, error) {
	return false, errMuteTable
}
func (failingMuteStore) ListMutes(context.Context, time.Time) ([]notify.Mute, error) {
	return nil, errMuteTable
}

func TestMuteHandlers_StoreFailureIs500WithoutInternals(t *testing.T) {
	ms := failingMuteStore{}
	for _, w := range []*httptest.ResponseRecorder{
		doRequestWithUser(createMuteHandler(ms, fixedNow), "POST", "/api/v1/notifications/mutes",
			`{"database":"orders","until":"2026-11-19T12:00:00Z"}`, muteAdmin),
		doRequest(listMutesHandler(ms, fixedNow), "GET", "/api/v1/notifications/mutes", ""),
		doRequest(deleteMuteHandler(ms), "DELETE", "/api/v1/notifications/mutes?database=o", ""),
	} {
		if w.Code != http.StatusInternalServerError ||
			strings.Contains(w.Body.String(), "relation") {
			t.Errorf("status %d %s, want 500 without the SQL error", w.Code, w.Body.String())
		}
	}
}

// Concurrency: two admins mute the same database at once through the API.
func TestCreateMuteHandler_ConcurrentAdminsLeaveOneMute(t *testing.T) {
	ms := notify.NewMemoryStore()
	h := createMuteHandler(ms, fixedNow)
	var wg sync.WaitGroup
	for _, until := range []string{"2026-11-19T12:00:00Z", "2026-11-20T12:00:00Z"} {
		wg.Add(1)
		go func(until string) {
			defer wg.Done()
			doRequestWithUser(h, "POST", "/api/v1/notifications/mutes",
				`{"database":"orders","until":"`+until+`"}`, muteAdmin)
		}(until)
	}
	wg.Wait()
	all, _ := ms.ListMutes(context.Background(), muteNow)
	if len(all) != 1 || !all[0].Until.Equal(time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("mutes %+v, want one with the later until", all)
	}
}

type fakeQuietSetter struct {
	id                int
	start, end, tz    string
	err               error
	calls             int
	returnNotFoundFor int
}

func (f *fakeQuietSetter) SetChannelQuietHours(_ context.Context, id int,
	start, end, tz string) error {
	f.calls++
	if id == f.returnNotFoundFor {
		return store.ErrNotFound
	}
	if f.err != nil {
		return f.err
	}
	f.id, f.start, f.end, f.tz = id, start, end, tz
	return nil
}

func putQuiet(h http.HandlerFunc, id, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("PUT", "/api/v1/notifications/channels/"+id+"/quiet-hours",
		strings.NewReader(body))
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestQuietHoursHandler_SetsValidatesAndClears(t *testing.T) {
	f := &fakeQuietSetter{returnNotFoundFor: 99}
	h := setChannelQuietHoursHandler(f)
	w := putQuiet(h, "3",
		`{"start":"22:00","end":"07:00","timezone":"America/Chicago"}`)
	if w.Code != http.StatusOK || f.id != 3 || f.start != "22:00" || f.end != "07:00" ||
		f.tz != "America/Chicago" {
		t.Fatalf("set: %d %s, stored %+v", w.Code, w.Body.String(), f)
	}
	if w = putQuiet(h, "3", `{"start":"","end":"","timezone":""}`); w.Code != http.StatusOK ||
		f.start != "" {
		t.Fatalf("clear: %d, stored %+v", w.Code, f)
	}
	calls := f.calls
	for _, c := range []struct{ id, body, want string }{
		{"x", `{}`, "invalid channel ID"},
		{"3", `not json`, "invalid request body"},
		{"3", `{"start":"22:00","end":"22:00","timezone":"UTC"}`, "equal"},
		{"3", `{"start":"22:00","end":"07:00","timezone":"Mars/Base"}`, "Mars/Base"},
	} {
		w = putQuiet(h, c.id, c.body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("%s %s: %d %s, want 400 naming %q", c.id, c.body, w.Code,
				w.Body.String(), c.want)
		}
	}
	if f.calls != calls {
		t.Fatalf("a refused window reached the store (%d calls)", f.calls-calls)
	}
	if w = putQuiet(h, "99", `{"start":"22:00","end":"07:00"}`); w.Code != http.StatusNotFound {
		t.Fatalf("unknown channel: %d, want 404", w.Code)
	}
}

type fakeWeekly struct {
	line notify.WeeklyLine
	err  error
	at   time.Time
}

func (f *fakeWeekly) WeeklyLine(_ context.Context, now time.Time) (notify.WeeklyLine, error) {
	f.at = now
	return f.line, f.err
}

func TestWeeklyLineHandler(t *testing.T) {
	f := &fakeWeekly{line: notify.ComputeWeeklyLine(nil, nil, muteNow)}
	w := doRequest(weeklyLineHandler(f, fixedNow), "GET",
		"/api/v1/notifications/weekly-line", "")
	got := decodeMap(t, w)
	if w.Code != 200 || got["text"] != "No messages this week" || got["messages"] != 0.0 ||
		!f.at.Equal(muteNow) {
		t.Fatalf("weekly line %d %v", w.Code, got)
	}
	f.err = errors.New("canceling statement due to statement timeout")
	w = doRequest(weeklyLineHandler(f, fixedNow), "GET", "/api/v1/notifications/weekly-line", "")
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "statement") {
		t.Fatalf("failed read: %d %s, want 500 without internals", w.Code, w.Body.String())
	}
}
