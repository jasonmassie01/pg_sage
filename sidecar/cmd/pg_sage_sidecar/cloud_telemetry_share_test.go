package main

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/managedparam"
)

// clouds-08: databases on one provider instance share one telemetry
// runtime (one poller, one bill) for as long as any of them runs.

// countingSource counts provider polls and signals each one.
type countingSource struct {
	mu     sync.Mutex
	calls  int
	polled chan struct{}
}

func newCountingSource() *countingSource {
	return &countingSource{polled: make(chan struct{}, 64)}
}

func (s *countingSource) Provider() string { return "rds" }

func (s *countingSource) Collect(ctx context.Context, now time.Time) (cloudtel.Sample,
	error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	s.polled <- struct{}{}
	return cloudtel.Sample{CollectedAt: now}, ctx.Err()
}

func (s *countingSource) Target(context.Context) (managedparam.Target, error) {
	return managedparam.Target{Provider: "rds"}, nil
}

func (s *countingSource) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// shareFixture builds a polling runtime over one countingSource whose
// waits between polls are driven by ticks; builds counts the builds.
type shareFixture struct {
	src    *countingSource
	ticks  chan time.Time
	mu     sync.Mutex
	builds int
}

func newShareFixture() *shareFixture {
	return &shareFixture{src: newCountingSource(), ticks: make(chan time.Time)}
}

func (f *shareFixture) build(database string) cloudBuild {
	return func(ctx context.Context) (*cloudtel.Runtime, managedparam.Resolver) {
		f.mu.Lock()
		f.builds++
		f.mu.Unlock()
		rt, err := cloudtel.NewRuntime(f.src, cloudtel.RuntimeOptions{Database: database,
			Interval: time.Minute, Limits: cloudtel.DefaultLimits(),
			After: func(time.Duration) <-chan time.Time { return f.ticks }})
		if err != nil {
			panic(err)
		}
		return rt, f.src
	}
}

func (f *shareFixture) buildCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.builds
}

func waitPoll(t *testing.T, f *shareFixture) {
	t.Helper()
	select {
	case <-f.src.polled:
	case <-time.After(5 * time.Second):
		t.Fatal("the shared runtime did not poll")
	}
}

// tick lets Run's wait end, then waits for the poll it starts.
func tick(t *testing.T, f *shareFixture) {
	t.Helper()
	select {
	case f.ticks <- time.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("Run is not waiting for its next poll")
	}
	waitPoll(t, f)
}

func noMorePolls(t *testing.T, f *shareFixture) {
	t.Helper()
	select {
	case <-f.src.polled:
		t.Fatal("an extra poll ran")
	case f.ticks <- time.Now():
		t.Fatal("a stopped runtime still waits for its next poll")
	case <-time.After(100 * time.Millisecond):
	}
}

func statusOf(member string) (cloudtel.Status, int) {
	var found cloudtel.Status
	n := 0
	for _, st := range cloudtel.Statuses() {
		if slices.Contains(st.Databases, member) {
			found, n = st, n+1
		}
	}
	return found, n
}

func TestCloudTelemetryShareOnePollerForTwoDatabases(t *testing.T) {
	share, f := newCloudTelemetryShare(), newShareFixture()
	rtA, resA, releaseA := share.acquire("rds|i1", "share-cmd-a", f.build("share-cmd-a"))
	rtB, resB, releaseB := share.acquire("rds|i1", "share-cmd-b", f.build("share-cmd-b"))
	t.Cleanup(func() { releaseA(); releaseB() })
	if rtA == nil || rtA != rtB || resA != resB || resA == nil || f.buildCount() != 1 {
		t.Fatalf("not shared: %p %p, builds %d", rtA, rtB, f.buildCount())
	}
	waitPoll(t, f)
	tick(t, f)
	if got := f.src.count(); got != 2 {
		t.Fatalf("2 intervals polled %d times, want 2", got)
	}
	st, n := statusOf("share-cmd-a")
	if n != 1 || !slices.Equal(st.Databases, []string{"share-cmd-a", "share-cmd-b"}) {
		t.Fatalf("status listed %d times with members %v", n, st.Databases)
	}
	if cloudtel.Lookup("share-cmd-b") != rtA {
		t.Fatal("Lookup(b) is not the shared runtime")
	}
}

// CHECK-02: three member databases over ten intervals poll ten times.
func TestCloudTelemetryShareTenIntervalsThreeMembers(t *testing.T) {
	share, f := newCloudTelemetryShare(), newShareFixture()
	for _, db := range []string{"share-ten-a", "share-ten-b", "share-ten-c"} {
		_, _, release := share.acquire("rds|ten", db, f.build(db))
		t.Cleanup(release)
	}
	waitPoll(t, f)
	for range 9 {
		tick(t, f)
	}
	if got := f.src.count(); got != 10 {
		t.Fatalf("10 intervals with 3 members polled %d times, want 10", got)
	}
}

// State transition: stopping a keeps polling for b; stopping b stops it,
// and the source sees no further calls.
func TestCloudTelemetryShareLastReleaseStops(t *testing.T) {
	share, f := newCloudTelemetryShare(), newShareFixture()
	rt, _, releaseA := share.acquire("rds|stop", "share-stop-a", f.build("share-stop-a"))
	_, _, releaseB := share.acquire("rds|stop", "share-stop-b", f.build("share-stop-b"))
	waitPoll(t, f)
	releaseA()
	releaseA() // idempotent: must not release b's share
	tick(t, f)
	if cloudtel.Lookup("share-stop-a") != nil || cloudtel.Lookup("share-stop-b") != rt {
		t.Fatal("after a stops, a must be gone and b still served")
	}
	releaseB()
	calls := f.src.count()
	noMorePolls(t, f)
	if f.src.count() != calls || cloudtel.Lookup("share-stop-b") != nil {
		t.Fatalf("after the last release: calls %d -> %d, lookup %v", calls,
			f.src.count(), cloudtel.Lookup("share-stop-b"))
	}
	if _, n := statusOf("share-stop-b"); n != 0 || share.size() != 0 {
		t.Fatalf("stopped instance still listed (%d) or kept (%d)", n, share.size())
	}
	// A later acquire builds a fresh runtime.
	_, _, again := share.acquire("rds|stop", "share-stop-a", f.build("share-stop-a"))
	t.Cleanup(again)
	if f.buildCount() != 2 {
		t.Fatalf("re-acquire after the last release built %d times, want 2", f.buildCount())
	}
}

// A reload builds the database's next runtime generation before the old
// one stops. When the database is the instance's only member, the new
// generation rebuilds the runtime (fresh credentials, an immediate poll),
// as before sharing; the old one stops with the old generation.
func TestCloudTelemetryShareReloadOfOnlyMemberRebuilds(t *testing.T) {
	share, f := newCloudTelemetryShare(), newShareFixture()
	old, _, gen1 := share.acquire("rds|gen", "share-gen", f.build("share-gen"))
	waitPoll(t, f)
	fresh, _, gen2 := share.acquire("rds|gen", "share-gen", f.build("share-gen"))
	t.Cleanup(gen2)
	waitPoll(t, f) // the rebuilt runtime polls at once
	if fresh == old || f.buildCount() != 2 || cloudtel.Lookup("share-gen") != fresh {
		t.Fatalf("reload reused the runtime: builds %d", f.buildCount())
	}
	gen1()
	if cloudtel.Lookup("share-gen") != fresh || share.size() != 1 {
		t.Fatal("the old generation's release dropped the new one")
	}
}

// With other members the runtime is kept (they hold it), and the reloaded
// member's new generation makes it poll at once.
func TestCloudTelemetryShareReloadOfOneMemberPollsNow(t *testing.T) {
	share, f := newCloudTelemetryShare(), newShareFixture()
	rt, _, a := share.acquire("rds|genm", "share-genm-a", f.build("share-genm-a"))
	_, _, b1 := share.acquire("rds|genm", "share-genm-b", f.build("share-genm-b"))
	t.Cleanup(a)
	waitPoll(t, f)
	again, _, b2 := share.acquire("rds|genm", "share-genm-b", f.build("share-genm-b"))
	t.Cleanup(b2)
	waitPoll(t, f) // no tick: the reload woke the poller
	if again != rt || f.buildCount() != 1 {
		t.Fatalf("a member's reload rebuilt a shared runtime: builds %d", f.buildCount())
	}
	b1()
	if cloudtel.Lookup("share-genm-b") != rt {
		t.Fatal("the old generation's release unregistered the member")
	}
}

// Concurrency: 10 goroutines acquiring one key build and run once.
func TestCloudTelemetryShareConcurrentAcquireRunsOnce(t *testing.T) {
	share, f := newCloudTelemetryShare(), newShareFixture()
	rts := make([]*cloudtel.Runtime, 10)
	releases := make([]func(), 10)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			db := fmt.Sprintf("share-race-cmd-%d", i)
			rts[i], _, releases[i] = share.acquire("rds|race", db, f.build(db))
		}()
	}
	close(start)
	wg.Wait()
	waitPoll(t, f)
	for i, rt := range rts {
		if rt == nil || rt != rts[0] {
			t.Fatalf("goroutine %d got runtime %p, want %p", i, rt, rts[0])
		}
	}
	if f.buildCount() != 1 {
		t.Fatalf("10 concurrent acquires built %d runtimes, want 1", f.buildCount())
	}
	select { // a second Run would poll at once, without a tick
	case <-f.src.polled:
		t.Fatal("a second Run polled")
	case <-time.After(100 * time.Millisecond):
	}
	for _, release := range releases {
		release()
	}
	if share.size() != 0 {
		t.Fatalf("share kept %d entries after every release", share.size())
	}
}

// Nil: a provider without telemetry builds nothing and shares nothing.
func TestCloudTelemetryShareNilBuild(t *testing.T) {
	share := newCloudTelemetryShare()
	builds := 0
	none := func(context.Context) (*cloudtel.Runtime, managedparam.Resolver) {
		builds++
		return nil, nil
	}
	rt, res, release := share.acquire("self-managed|x", "share-nil", none)
	if rt != nil || res != nil || release == nil || share.size() != 0 {
		t.Fatalf("nil build = %v %v, size %d", rt, res, share.size())
	}
	release()
	share.acquire("self-managed|x", "share-nil", none)
	if builds != 2 {
		t.Fatalf("a nil build was cached (%d builds)", builds)
	}
}

// The build runs under the share's context, not the first member's, so
// the first database stopping cannot cancel credentials the others use.
func TestCloudTelemetryShareBuildContextOutlivesFirstMember(t *testing.T) {
	share, f := newCloudTelemetryShare(), newShareFixture()
	var buildCtx context.Context
	build := func(ctx context.Context) (*cloudtel.Runtime, managedparam.Resolver) {
		buildCtx = ctx
		return f.build("share-ctx-a")(ctx)
	}
	_, _, releaseA := share.acquire("rds|ctx", "share-ctx-a", build)
	_, _, releaseB := share.acquire("rds|ctx", "share-ctx-b", f.build("share-ctx-b"))
	waitPoll(t, f)
	releaseA()
	if buildCtx.Err() != nil {
		t.Fatal("the first member's release cancelled the shared build context")
	}
	releaseB()
	if buildCtx.Err() == nil {
		t.Fatal("the last release left the shared context running")
	}
}

func shareKey(provider, host string, port uint16, r config.CloudResourceConfig) string {
	return cloudShareKey(provider, host, port, r, config.DefaultConfig().CloudTelemetry)
}

// Boundary: the key names one instance; different instances, ports,
// providers or polling settings never merge.
func TestCloudTelemetryShareKeys(t *testing.T) {
	const rds = "orders.c9akciq32.us-east-1.rds.amazonaws.com"
	none := config.CloudResourceConfig{}
	named := config.CloudResourceConfig{AWS: config.CloudResourceAWS{
		DBInstanceIdentifier: "orders", Region: "us-east-1"}}
	gcp := config.CloudResourceConfig{GCP: config.CloudResourceGCP{Project: "p",
		Instance: "main"}}
	same := [][2]string{
		{shareKey("rds", rds, 5432, none), shareKey("rds", rds, 5432, none)},
		{shareKey("rds", rds, 5432, none), shareKey("rds", "ORDERS.c9akciq32.us-east-1.rds."+
			"amazonaws.com", 5432, none)},
		{shareKey("rds", "10.0.0.5", 5432, named), shareKey("rds", "10.0.0.6", 6432, named)},
		{shareKey("cloud-sql", "127.0.0.1", 5432, gcp),
			shareKey("cloud-sql", "127.0.0.2", 5433, gcp)},
	}
	for i, pair := range same {
		if pair[0] != pair[1] {
			t.Errorf("same instance %d: %q != %q", i, pair[0], pair[1])
		}
	}
	slow := config.DefaultConfig().CloudTelemetry
	slow.PollIntervalSeconds = 300
	differ := [][2]string{
		{shareKey("rds", rds, 5432, none), shareKey("rds",
			"billing.c9akciq32.us-east-1.rds.amazonaws.com", 5432, none)},
		{shareKey("rds", "10.0.0.5", 5432, none), shareKey("rds", "10.0.0.5", 5433, none)},
		{shareKey("rds", "10.0.0.5", 5432, none), shareKey("aurora", "10.0.0.5", 5432, none)},
		// An RDS endpoint host wins over a configured name, as in the build.
		{shareKey("rds", rds, 5432, named), shareKey("rds", "10.0.0.5", 5432, named)},
		{shareKey("rds", "10.0.0.5", 5432, named), cloudShareKey("rds", "10.0.0.5", 5432,
			named, slow)},
	}
	for i, pair := range differ {
		if pair[0] == pair[1] {
			t.Errorf("different instances %d share key %q", i, pair[0])
		}
	}
}

// A reload whose candidate is discarded (another database of the reload
// failed) releases the rebuilt runtime first: the running generation's
// runtime must come back as the instance's entry and status, or the next
// member would start a second poller and the API would lose the database.
func TestCloudTelemetryShareDiscardedReloadRestoresTheRunningRuntime(t *testing.T) {
	share, f := newCloudTelemetryShare(), newShareFixture()
	old, _, gen1 := share.acquire("rds|discard", "share-discard", f.build("share-discard"))
	waitPoll(t, f)
	fresh, _, gen2 := share.acquire("rds|discard", "share-discard",
		f.build("share-discard"))
	waitPoll(t, f)
	if fresh == old {
		t.Fatal("the reload did not rebuild the only member's runtime")
	}
	gen2() // the candidate is discarded
	if cloudtel.Lookup("share-discard") != old || share.size() != 1 {
		t.Fatalf("after the discard: lookup is the running runtime %v, entries %d",
			cloudtel.Lookup("share-discard") == old, share.size())
	}
	joined, _, other := share.acquire("rds|discard", "share-discard-b",
		f.build("share-discard-b"))
	if joined != old || f.buildCount() != 2 {
		t.Fatalf("a new member got its own runtime (builds %d)", f.buildCount())
	}
	gen1()
	other()
	if share.size() != 0 || cloudtel.Lookup("share-discard") != nil {
		t.Fatalf("after every release: entries %d", share.size())
	}
}
