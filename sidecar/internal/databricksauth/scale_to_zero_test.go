package databricksauth

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/databricks/databricks-sdk-go/apierr"
	"github.com/databricks/databricks-sdk-go/common/types/duration"
	"github.com/databricks/databricks-sdk-go/service/postgres"
)

// resetEndpointInfos isolates a test from endpoint info other tests recorded.
func resetEndpointInfos(t *testing.T) {
	t.Helper()
	clear := func() {
		endpointInfos.mu.Lock()
		endpointInfos.m = map[string]EndpointInfo{}
		endpointInfos.mu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

func suspending(after time.Duration) EndpointInfo {
	return EndpointInfo{SuspendTimeout: after, State: "ACTIVE"}
}

func scaleEndpoint(name, host string, suspend *duration.Duration,
	state postgres.EndpointStatusState) *postgres.Endpoint {
	ep := directEndpoint(name, host)
	ep.Status.SuspendTimeoutDuration = suspend
	ep.Status.CurrentState = state
	return ep
}

func configureEndpointA(t *testing.T, ep *postgres.Endpoint) error {
	t.Helper()
	fake := &fakeEndpointAPI{endpoints: map[string]*postgres.Endpoint{endpointA: ep}}
	installFakeAPI(t, fake)
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS", "")
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", endpointA)
	return ConfigurePool(t.Context(), poolFor(t, "a.example"))
}

// Happy path: the monitored pool's defaults (60 s collector, MinConns 1, 30 s
// health check) are all shorter than a 300 s suspend timeout.
func TestScaleToZeroBlockersNameEveryShorterCadence(t *testing.T) {
	got := ScaleToZeroBlockers(suspending(300*time.Second), time.Minute, 1, 30*time.Second)
	want := []string{"collector every 1m0s", "health check every 30s",
		"min_conns=1 keeps a connection open"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("blockers = %q; want %q", got, want)
	}
}

// The health check only touches the server while the pool keeps connections,
// so MinConns 0 drops both the health check and min_conns blockers.
func TestScaleToZeroBlockersIgnoreHealthCheckWithoutMinConns(t *testing.T) {
	got := ScaleToZeroBlockers(suspending(300*time.Second), time.Minute, 0, 30*time.Second)
	if !reflect.DeepEqual(got, []string{"collector every 1m0s"}) {
		t.Fatalf("blockers = %q; want only the collector", got)
	}
}

// Nil/empty: no suspend timeout (unreported or scale-to-zero off) has nothing
// to protect, whatever pg_sage's cadence.
func TestScaleToZeroBlockersWithoutSuspendTimeoutAreEmpty(t *testing.T) {
	for name, info := range map[string]EndpointInfo{
		"zero value": {}, "off": {SuspendTimeout: 0, State: "ACTIVE"},
	} {
		if got := ScaleToZeroBlockers(info, time.Second, 5, time.Second); len(got) != 0 {
			t.Fatalf("%s: blockers = %q; want none", name, got)
		}
	}
}

// Boundary: a cadence must be strictly shorter than the suspend timeout.
func TestScaleToZeroCollectorBoundary(t *testing.T) {
	cases := []struct {
		suspend time.Duration
		want    bool
	}{{59 * time.Second, false}, {60 * time.Second, false}, {61 * time.Second, true}}
	for _, tc := range cases {
		got := ScaleToZeroBlockers(suspending(tc.suspend), time.Minute, 0, 0)
		want := []string(nil)
		if tc.want {
			want = []string{"collector every 1m0s"}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("suspend %s with a 60s collector = %q; want reported=%v",
				tc.suspend, got, tc.want)
		}
	}
}

// Boundary on the health check, and an unknown (zero) cadence is not a blocker.
func TestScaleToZeroHealthCheckBoundaryAndUnknownCadences(t *testing.T) {
	info := suspending(30 * time.Second)
	if got := ScaleToZeroBlockers(info, 0, 1, 30*time.Second); !reflect.DeepEqual(got,
		[]string{"min_conns=1 keeps a connection open"}) {
		t.Fatalf("30s health check vs 30s suspend = %q; want only min_conns", got)
	}
	if got := ScaleToZeroBlockers(info, -time.Second, 0, -time.Second); len(got) != 0 {
		t.Fatalf("negative cadences = %q; want none", got)
	}
}

// ConfigurePool records what the existing GetEndpoint call already returned.
func TestConfigurePoolRecordsEndpointInfo(t *testing.T) {
	resetEndpointInfos(t)
	before := time.Now()
	err := configureEndpointA(t, scaleEndpoint(endpointA, "a.example",
		duration.New(5*time.Minute), postgres.EndpointStatusStateIdle))
	if err != nil {
		t.Fatal(err)
	}
	info, ok := EndpointInfoFor(endpointA)
	if !ok {
		t.Fatal("no endpoint info recorded after a successful ConfigurePool")
	}
	if info.SuspendTimeout != 5*time.Minute || info.State != "IDLE" {
		t.Fatalf("info = %+v; want 5m suspend timeout and IDLE", info)
	}
	if info.ObservedAt.Before(before) || info.ObservedAt.After(time.Now()) {
		t.Fatalf("observed at %s; want during the call", info.ObservedAt)
	}
	endpoint, byHost, ok := EndpointInfoForHost("a.example")
	if !ok || endpoint != endpointA || byHost.SuspendTimeout != info.SuspendTimeout {
		t.Fatalf("by host = %q %+v %v; want %s's info", endpoint, byHost, ok, endpointA)
	}
}

// Nil/empty and invalid input: a nil, zero or negative suspend timeout is
// recorded as 0, which gives no blockers.
func TestConfigurePoolTreatsMissingOrNegativeSuspendAsUnknown(t *testing.T) {
	cases := map[string]*duration.Duration{
		"nil": nil, "zero": duration.New(0), "negative": duration.New(-time.Minute),
	}
	for name, suspend := range cases {
		t.Run(name, func(t *testing.T) {
			resetEndpointInfos(t)
			if err := configureEndpointA(t, scaleEndpoint(endpointA, "a.example", suspend,
				postgres.EndpointStatusStateActive)); err != nil {
				t.Fatal(err)
			}
			info, ok := EndpointInfoFor(endpointA)
			if !ok || info.SuspendTimeout != 0 || info.State != "ACTIVE" {
				t.Fatalf("info = %+v, %v; want recorded with no suspend timeout", info, ok)
			}
			if got := ScaleToZeroBlockers(info, time.Second, 1, time.Second); len(got) != 0 {
				t.Fatalf("blockers = %q; want none", got)
			}
		})
	}
}

// Error propagation: a GetEndpoint failure keeps the existing error and
// stores nothing, and an endpoint without hosts stores nothing either.
func TestFailedResolveStoresNoEndpointInfo(t *testing.T) {
	resetEndpointInfos(t)
	fake := &fakeEndpointAPI{getErrs: []error{&apierr.APIError{StatusCode: 503,
		Message: "unavailable"}}}
	installFakeAPI(t, fake)
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS", "")
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", endpointA)
	err := ConfigurePool(t.Context(), poolFor(t, "a.example"))
	if err == nil || !strings.Contains(err.Error(), "resolve Lakebase endpoint "+endpointA) {
		t.Fatalf("error = %v; want the resolve error", err)
	}
	if info, ok := EndpointInfoFor(endpointA); ok {
		t.Fatalf("failed resolve stored %+v", info)
	}
	err = configureEndpointA(t, &postgres.Endpoint{Name: endpointA,
		Status: &postgres.EndpointStatus{SuspendTimeoutDuration: duration.New(time.Hour)}})
	if err == nil || !strings.Contains(err.Error(), "no connection hosts") {
		t.Fatalf("endpoint without hosts = %v", err)
	}
	if info, ok := EndpointInfoFor(endpointA); ok {
		t.Fatalf("endpoint without hosts stored %+v", info)
	}
	if _, _, ok := EndpointInfoForHost("a.example"); ok {
		t.Fatal("host lookup found info that was never recorded")
	}
}

// A pooled-host refusal happens after GetEndpoint but must store nothing.
func TestRefusedPoolStoresNoEndpointInfo(t *testing.T) {
	resetEndpointInfos(t)
	ep := scaleEndpoint(endpointA, "a.example", duration.New(time.Hour), "ACTIVE")
	ep.Status.Hosts.ReadWritePooledHost = "a.example"
	ep.Status.Hosts.Host = "direct.example"
	if err := configureEndpointA(t, ep); err == nil {
		t.Fatal("pooled host accepted")
	}
	if info, ok := EndpointInfoFor(endpointA); ok {
		t.Fatalf("refused pool stored %+v", info)
	}
}

// Ordinary PostgreSQL hosts have no endpoint and no info.
func TestEndpointInfoForUnmappedHost(t *testing.T) {
	resetEndpointInfos(t)
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", "")
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS", "")
	if endpoint, info, ok := EndpointInfoForHost("db.example"); ok || endpoint != "" ||
		!reflect.DeepEqual(info, EndpointInfo{}) {
		t.Fatalf("unmapped host = %q %+v %v; want nothing", endpoint, info, ok)
	}
}

// State transitions: noting blockers returns only the new ones, so a
// reconnect does not warn again, and a later re-resolve keeps them.
func TestNoteScaleToZeroBlockersReturnsOnlyNewOnes(t *testing.T) {
	resetEndpointInfos(t)
	if added := NoteScaleToZeroBlockers(endpointA, []string{"x"}); len(added) != 0 {
		t.Fatalf("noted %q before any endpoint info; want nothing", added)
	}
	if err := configureEndpointA(t, scaleEndpoint(endpointA, "a.example",
		duration.New(time.Hour), "ACTIVE")); err != nil {
		t.Fatal(err)
	}
	first := NoteScaleToZeroBlockers(endpointA, []string{"min_conns=1 keeps a connection open"})
	second := NoteScaleToZeroBlockers(endpointA, []string{"collector every 1m0s",
		"min_conns=1 keeps a connection open"})
	if !reflect.DeepEqual(first, []string{"min_conns=1 keeps a connection open"}) ||
		!reflect.DeepEqual(second, []string{"collector every 1m0s"}) {
		t.Fatalf("added = %q then %q; want each blocker once", first, second)
	}
	if again := NoteScaleToZeroBlockers(endpointA, second); len(again) != 0 {
		t.Fatalf("repeat note added %q", again)
	}
	if err := configureEndpointA(t, scaleEndpoint(endpointA, "a.example",
		duration.New(time.Hour), "IDLE")); err != nil {
		t.Fatal(err)
	}
	info, _ := EndpointInfoFor(endpointA)
	want := []string{"min_conns=1 keeps a connection open", "collector every 1m0s"}
	if info.State != "IDLE" || !reflect.DeepEqual(info.Blockers, want) {
		t.Fatalf("after re-resolve info = %+v; want IDLE and blockers %q", info, want)
	}
	info.Blockers[0] = "mutated"
	if fresh, _ := EndpointInfoFor(endpointA); fresh.Blockers[0] == "mutated" {
		t.Fatal("EndpointInfoFor returned the stored slice, not a copy")
	}
}

// Concurrent access: fleet pools note blockers for one endpoint at once;
// every distinct blocker is returned to exactly one caller.
func TestNoteScaleToZeroBlockersConcurrently(t *testing.T) {
	resetEndpointInfos(t)
	recordEndpointInfo(endpointA, suspending(time.Hour))
	var wg sync.WaitGroup
	results := make(chan []string, 40)
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- NoteScaleToZeroBlockers(endpointA, []string{fmt.Sprintf("b%d", i%8)})
		}()
	}
	wg.Wait()
	close(results)
	seen := map[string]int{}
	for added := range results {
		for _, b := range added {
			seen[b]++
		}
	}
	info, _ := EndpointInfoFor(endpointA)
	if len(seen) != 8 || len(info.Blockers) != 8 {
		t.Fatalf("seen %v, stored %q; want 8 distinct blockers", seen, info.Blockers)
	}
	for b, n := range seen {
		if n != 1 {
			t.Fatalf("blocker %s returned %d times; want once", b, n)
		}
	}
}

func TestEndpointInfoForUnknownEndpoint(t *testing.T) {
	resetEndpointInfos(t)
	if info, ok := EndpointInfoFor("projects/p/branches/b/endpoints/none"); ok ||
		!reflect.DeepEqual(info, EndpointInfo{}) {
		t.Fatalf("unknown endpoint = %+v, %v; want nothing", info, ok)
	}
}
