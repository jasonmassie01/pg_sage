package changefeed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Provider events (guard-02b): a server version change, a host change and
// a restart land in the change feed, labelled operator, provider or
// unknown. These tests drive the sources through a fake querier; the
// _db_test reads the real catalog functions.

// fakeRow answers one QueryRow with fixed values or an error.
type fakeRow struct {
	vals []any
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.vals) {
		return fmt.Errorf("fake row: %d destinations for %d values", len(dest), len(r.vals))
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *string:
			*p = r.vals[i].(string)
		case **string:
			if r.vals[i] == nil {
				*p = nil
			} else {
				v := r.vals[i].(string)
				*p = &v
			}
		case *time.Time:
			*p = r.vals[i].(time.Time)
		case *int64:
			*p = r.vals[i].(int64)
		case *bool:
			*p = r.vals[i].(bool)
		default:
			return fmt.Errorf("fake row: unsupported destination %T", d)
		}
	}
	return nil
}

// fakeQ answers QueryRow by the first SQL fragment it contains.
type fakeQ struct {
	mu   sync.Mutex
	rows map[string]fakeRow
	seen []string
}

func (q *fakeQ) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("fake querier: Query not supported")
}

func (q *fakeQ) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.seen = append(q.seen, sql)
	for frag, row := range q.rows {
		if strings.Contains(sql, frag) {
			return row
		}
	}
	return fakeRow{err: fmt.Errorf("fake querier: no answer for %q", sql)}
}

func (q *fakeQ) set(frag string, row fakeRow) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.rows[frag] = row
}

const (
	versionSQL = "server_version_num"
	addrSQL    = "inet_server_addr"
	sysidSQL   = "pg_control_system"
	startSQL   = "SELECT pg_postmaster_start_time()"
)

var (
	start1 = time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
	start2 = time.Date(2026, 11, 2, 3, 4, 0, 0, time.UTC)
	pollAt = time.Date(2026, 11, 2, 3, 5, 0, 0, time.UTC)
)

func versionQ(num string, start time.Time) *fakeQ {
	return &fakeQ{rows: map[string]fakeRow{
		versionSQL: {vals: []any{num, start}},
		startSQL:   {vals: []any{start}},
	}}
}

type logLine struct{ level, msg string }

func capturePoller(t *testing.T) (*Poller, *[]logLine) {
	t.Helper()
	var mu sync.Mutex
	var lines []logLine
	p := &Poller{logf: func(level, msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, logLine{level, fmt.Sprintf(msg, args...)})
	}, now: func() time.Time { return pollAt }}
	return p, &lines
}

func mustState(t *testing.T, state any) []byte {
	t.Helper()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("encode state: %v", err)
	}
	return raw
}

// Happy path: 17.4 -> 17.5 across a restart is one minor version_change,
// dated by the new postmaster start, keyed by the change and the start.
func TestProviderEvent_VersionMinorChange(t *testing.T) {
	p, _ := capturePoller(t)
	ctx := context.Background()
	_, prev, err := p.pollServerVersion(ctx, versionQ("170004", start1), nil, pollAt)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	evs, state, err := p.pollServerVersion(ctx, versionQ("170005", start2),
		mustState(t, prev), pollAt)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("events = %+v, want one version_change", evs)
	}
	e := evs[0]
	if e.Kind != KindVersionChange || e.Source != SourceSage ||
		e.Signature != SignatureInternal || !e.Scoped {
		t.Fatalf("event = %+v", e)
	}
	if !strings.Contains(e.Summary, "17.4 -> 17.5") || !strings.Contains(e.Summary, "minor") {
		t.Fatalf("summary = %q, want 17.4 -> 17.5 and minor", e.Summary)
	}
	if !strings.HasPrefix(e.EventID, "version:minor:170004->170005") {
		t.Fatalf("event id = %q", e.EventID)
	}
	if !e.OccurredAt.Equal(start2) {
		t.Fatalf("occurred at %v, want the new postmaster start %v", e.OccurredAt, start2)
	}
	if VersionChangeMajor(e) {
		t.Fatal("a minor change reads as major")
	}
	st := state.(versionState)
	if st.Num != 170005 {
		t.Fatalf("state = %+v, want 170005", st)
	}
}

// A major upgrade (16 -> 17) says major; drill-02c reads it.
func TestProviderEvent_VersionMajorChange(t *testing.T) {
	p, _ := capturePoller(t)
	prev := mustState(t, versionState{Num: 160008, Start: start1.Format(time.RFC3339Nano)})
	evs, _, err := p.pollServerVersion(context.Background(), versionQ("170000", start2),
		prev, pollAt)
	if err != nil || len(evs) != 1 {
		t.Fatalf("events = %+v, err = %v", evs, err)
	}
	if !VersionChangeMajor(evs[0]) || !strings.Contains(evs[0].Summary, "16.8 -> 17.0") ||
		!strings.Contains(evs[0].Summary, "major") {
		t.Fatalf("event = %+v, want a major 16.8 -> 17.0", evs[0])
	}
	if VersionChangeMajor(Event{Kind: KindRestart, EventID: "version:major:1->2"}) {
		t.Fatal("a restart event reads as a major version change")
	}
}

// Pre-10 numbering: 90624 is 9.6.24, and 9.6 -> 10 is major.
func TestProviderEvent_VersionFormatting(t *testing.T) {
	cases := map[int]string{90624: "9.6.24", 100023: "10.23", 170005: "17.5",
		180000: "18.0"}
	for num, want := range cases {
		if got := formatVersion(num); got != want {
			t.Errorf("formatVersion(%d) = %q, want %q", num, got, want)
		}
	}
	if versionMajor(90624) == versionMajor(90500) {
		t.Error("9.6 and 9.5 share a major")
	}
	if versionMajor(170004) != versionMajor(170005) {
		t.Error("17.4 and 17.5 differ in major")
	}
}

// Invalid input: a non-numeric or out-of-range server_version_num is
// ignored with a logged reason; no event, no state.
func TestProviderEvent_VersionInvalidIgnored(t *testing.T) {
	prev := mustState(t, versionState{Num: 170004, Start: start1.Format(time.RFC3339Nano)})
	for _, raw := range []string{"abc", "", "17.4", "-170004", "0", "99999999"} {
		p, lines := capturePoller(t)
		evs, state, err := p.pollServerVersion(context.Background(),
			versionQ(raw, start2), prev, pollAt)
		if !errors.Is(err, errInvalidValue) {
			t.Errorf("%q: err = %v, want errInvalidValue", raw, err)
		}
		if len(evs) != 0 || state != nil {
			t.Errorf("%q: events=%v state=%v, want none", raw, evs, state)
		}
		if classify(err) != "invalid_value" {
			t.Errorf("%q: classified %q", raw, classify(err))
		}
		if len(*lines) != 1 || (*lines)[0].level != "WARN" ||
			!strings.Contains((*lines)[0].msg, "server_version_num") {
			t.Errorf("%q: logged %+v, want one WARN naming server_version_num", raw, *lines)
		}
	}
}

// Nil/empty: the first poll records the baseline and emits nothing; an
// unreadable state document is an error, never a silent re-baseline.
func TestProviderEvent_FirstPollRecordsBaseline(t *testing.T) {
	p, _ := capturePoller(t)
	evs, state, err := p.pollServerVersion(context.Background(),
		versionQ("170004", start1), nil, pollAt)
	if err != nil || len(evs) != 0 {
		t.Fatalf("baseline: events=%v err=%v", evs, err)
	}
	if st := state.(versionState); st.Num != 170004 ||
		st.Start != start1.Format(time.RFC3339Nano) {
		t.Fatalf("baseline state = %+v", st)
	}
	q := &fakeQ{rows: map[string]fakeRow{addrSQL: {vals: []any{"10.0.0.5"}},
		sysidSQL: {vals: []any{"7300000000000000001"}}}}
	evs, hs, err := pollServerHost(context.Background(), q, nil, pollAt)
	if err != nil || len(evs) != 0 {
		t.Fatalf("host baseline: events=%v err=%v", evs, err)
	}
	if h := hs.(hostState); h.Addr != "10.0.0.5" || h.SystemID != "7300000000000000001" {
		t.Fatalf("host baseline = %+v", h)
	}
	if _, _, err := p.pollServerVersion(context.Background(), versionQ("170004", start1),
		[]byte(`not json`), pollAt); err == nil {
		t.Fatal("a corrupt version state was accepted")
	}
	if _, _, err := pollServerHost(context.Background(), q, []byte(`[`), pollAt); err == nil {
		t.Fatal("a corrupt host state was accepted")
	}
}

// A host change: a new server address and a new system identifier.
func TestProviderEvent_HostChange(t *testing.T) {
	prev := mustState(t, hostState{Addr: "10.0.0.5", SystemID: "1"})
	q := &fakeQ{rows: map[string]fakeRow{addrSQL: {vals: []any{"10.0.0.9"}},
		sysidSQL: {vals: []any{"2"}}}}
	evs, state, err := pollServerHost(context.Background(), q, prev, pollAt)
	if err != nil || len(evs) != 1 {
		t.Fatalf("events = %+v, err = %v", evs, err)
	}
	e := evs[0]
	if e.Kind != KindHostChange || !strings.Contains(e.Summary, "10.0.0.5 -> 10.0.0.9") ||
		!strings.Contains(e.Summary, "system identifier") || !e.OccurredAt.Equal(pollAt) {
		t.Fatalf("event = %+v", e)
	}
	st := state.(hostState)
	if st.Addr != "10.0.0.9" || st.SystemID != "2" || st.Changes != 1 {
		t.Fatalf("state = %+v", st)
	}
	// A flip back is a new event, not a replay of the first.
	q.set(addrSQL, fakeRow{vals: []any{"10.0.0.5"}})
	back, _, err := pollServerHost(context.Background(), q, mustState(t, st), pollAt)
	if err != nil || len(back) != 1 || back[0].EventID == e.EventID {
		t.Fatalf("flip back = %+v (first %q), err = %v", back, e.EventID, err)
	}
}

// Error propagation: permission denied on pg_control_system() keeps the
// host source on the address alone (and the last known identifier); a
// failing address read is the source's error.
func TestProviderEvent_HostWithoutControlSystemPrivilege(t *testing.T) {
	denied := &pgconn.PgError{Code: "42501", Message: "permission denied for function"}
	prev := mustState(t, hostState{Addr: "10.0.0.5", SystemID: "1"})
	q := &fakeQ{rows: map[string]fakeRow{addrSQL: {vals: []any{"10.0.0.5"}},
		sysidSQL: {err: denied}}}
	evs, state, err := pollServerHost(context.Background(), q, prev, pollAt)
	if err != nil || len(evs) != 0 {
		t.Fatalf("unchanged address without sysid: events=%v err=%v", evs, err)
	}
	if st := state.(hostState); st.Addr != "10.0.0.5" || st.SystemID != "1" {
		t.Fatalf("state = %+v, want the address and the last known identifier", st)
	}
	q.set(addrSQL, fakeRow{vals: []any{"10.0.0.7"}})
	evs, _, err = pollServerHost(context.Background(), q, prev, pollAt)
	if err != nil || len(evs) != 1 || strings.Contains(evs[0].Summary, "system identifier") {
		t.Fatalf("address-only change: events=%+v err=%v", evs, err)
	}
	broken := errors.New("connection reset by peer")
	q.set(addrSQL, fakeRow{err: broken})
	if _, _, err := pollServerHost(context.Background(), q, prev, pollAt); !errors.Is(err,
		broken) {
		t.Fatalf("address read failure: err = %v, want it propagated", err)
	}
	q.set(addrSQL, fakeRow{vals: []any{"10.0.0.5"}})
	other := errors.New("server closed the connection")
	q.set(sysidSQL, fakeRow{err: other})
	if _, _, err := pollServerHost(context.Background(), q, prev, pollAt); !errors.Is(err,
		other) {
		t.Fatalf("non-privilege sysid failure: err = %v, want it propagated", err)
	}
}

// A Unix-socket connection has no server address: no address change is
// invented when one side is unknown.
func TestProviderEvent_HostSocketIsNotAChange(t *testing.T) {
	prev := mustState(t, hostState{Addr: "", SystemID: "1"})
	q := &fakeQ{rows: map[string]fakeRow{addrSQL: {vals: []any{"10.0.0.5"}},
		sysidSQL: {vals: []any{"1"}}}}
	evs, _, err := pollServerHost(context.Background(), q, prev, pollAt)
	if err != nil || len(evs) != 0 {
		t.Fatalf("socket -> TCP: events=%v err=%v", evs, err)
	}
	q.set(addrSQL, fakeRow{vals: []any{nil}})
	evs, state, err := pollServerHost(context.Background(), q,
		mustState(t, hostState{Addr: "10.0.0.5", SystemID: "1"}), pollAt)
	if err != nil || len(evs) != 0 || state.(hostState).Addr != "" {
		t.Fatalf("TCP -> socket: events=%v state=%v err=%v", evs, state, err)
	}
}

// Boundary: an event 9m59s after an operator change is operator's, 10m01s
// is the provider's on a managed database and unknown when self-managed.
func TestProviderEvent_AttributionWindow(t *testing.T) {
	at := pollAt
	cases := []struct {
		name    string
		last    time.Time
		managed bool
		want    string
	}{
		{"9m59s managed", at.Add(-9*time.Minute - 59*time.Second), true, AttributionOperator},
		{"9m59s self-managed", at.Add(-9*time.Minute - 59*time.Second), false,
			AttributionOperator},
		{"exactly 10m", at.Add(-10 * time.Minute), true, AttributionOperator},
		{"10m01s managed", at.Add(-10*time.Minute - time.Second), true, AttributionProvider},
		{"10m01s self-managed", at.Add(-10*time.Minute - time.Second), false,
			AttributionUnknown},
		{"no change managed", time.Time{}, true, AttributionProvider},
		{"no change self-managed", time.Time{}, false, AttributionUnknown},
		{"change after the event", at.Add(time.Second), true, AttributionProvider},
	}
	for _, c := range cases {
		if got := attribution(at, c.last, c.managed); got != c.want {
			t.Errorf("%s: attribution = %q, want %q", c.name, got, c.want)
		}
	}
}

// Only provider-event kinds are attributed; the label is part of the hash
// only when set, so events recorded before guard-02b replay unchanged.
func TestProviderEvent_AttributedKindsAndHash(t *testing.T) {
	for k, want := range map[Kind]bool{KindRestart: true, KindFailover: true,
		KindVersionChange: true, KindHostChange: true, KindConfig: false,
		KindSageAction: false, KindExtension: false, KindDeploy: false} {
		if attributable(k) != want {
			t.Errorf("attributable(%s) = %v", k, !want)
		}
	}
	if KindVersionChange.External() || KindHostChange.External() {
		t.Fatal("provider-event kinds can be submitted by external systems")
	}
	e := internalEvent(KindRestart, "restart:x", "restarted", pollAt, pollAt)
	plain := e.Hash
	e.Attribution = AttributionProvider
	if hashOf(e) == plain {
		t.Fatal("attribution is not part of the hash")
	}
	e.Attribution = ""
	if hashOf(e) != plain {
		t.Fatal("an unattributed event hashes differently than before")
	}
}

// The provider_api source name is pg_sage's own: an external submission
// cannot claim it (and pre-empt a provider event id).
func TestProviderEvent_SubmissionCannotClaimProviderSource(t *testing.T) {
	s := validSubmission()
	s.Source = SourceProviderAPI
	if _, err := s.Event(time.Now()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("submission as %s: err = %v", SourceProviderAPI, err)
	}
}

// State transitions: stable -> restarted (restart event, no version
// change) -> version changed (one version_change).
func TestProviderEvent_StableRestartedVersionChanged(t *testing.T) {
	p, _ := capturePoller(t)
	ctx := context.Background()
	q := versionQ("170004", start1)
	_, vs, err := p.pollServerVersion(ctx, q, nil, pollAt)
	if err != nil {
		t.Fatal(err)
	}
	_, ps, err := pollPostmaster(ctx, q, nil, pollAt)
	if err != nil {
		t.Fatal(err)
	}
	step := func(name string, wantRestart, wantVersion int) {
		t.Helper()
		rev, nps, err := pollPostmaster(ctx, q, mustState(t, ps), pollAt)
		if err != nil {
			t.Fatalf("%s: postmaster: %v", name, err)
		}
		vev, nvs, err := p.pollServerVersion(ctx, q, mustState(t, vs), pollAt)
		if err != nil {
			t.Fatalf("%s: version: %v", name, err)
		}
		if len(rev) != wantRestart || len(vev) != wantVersion {
			t.Fatalf("%s: %d restart and %d version events, want %d and %d", name,
				len(rev), len(vev), wantRestart, wantVersion)
		}
		ps, vs = nps, nvs
	}
	step("stable", 0, 0)
	q.set(startSQL, fakeRow{vals: []any{start2}})
	q.set(versionSQL, fakeRow{vals: []any{"170004", start2}})
	step("restarted", 1, 0)
	step("stable after restart", 0, 0)
	start3 := start2.Add(time.Hour)
	q.set(startSQL, fakeRow{vals: []any{start3}})
	q.set(versionSQL, fakeRow{vals: []any{"170005", start3}})
	step("version changed", 1, 1)
	step("stable after upgrade", 0, 0)
}

// The provider-event sources alone run when feed polling is off; the full
// feed includes them.
func TestProviderEvent_SourcesIndependentOfFeed(t *testing.T) {
	names := func(p *Poller) map[string]bool {
		out := map[string]bool{}
		for _, s := range p.sources() {
			out[s.name] = true
		}
		return out
	}
	provider := []string{"postmaster", "recovery_role", "timeline", "server_version",
		"server_host"}
	only := names(&Poller{providerOnly: true})
	full := names(&Poller{})
	for _, n := range provider {
		if !only[n] || !full[n] {
			t.Errorf("source %s: provider-only %v, full %v", n, only[n], full[n])
		}
	}
	if len(only) != len(provider) {
		t.Errorf("provider-only sources = %v, want exactly %v", only, provider)
	}
	for _, n := range []string{"sage_actions", "config_audit", "extensions"} {
		if only[n] || !full[n] {
			t.Errorf("source %s: provider-only %v, full %v", n, only[n], full[n])
		}
	}
}

// No concurrent-access unit test here: the source functions are pure over
// their querier and previous state; two pollers racing on one state row
// are exercised against PostgreSQL in provider_events_db_test.go.
