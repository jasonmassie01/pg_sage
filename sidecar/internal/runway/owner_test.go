package runway

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// forecast-09: one runtime per cluster reports the cluster's series in
// fleet mode. These tests drive the election and the scope without a
// database; owner_db_test.go drives three databases on real PostgreSQL.

// ownerClock is a settable clock for a ClusterOwners.
type ownerClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *ownerClock) get() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *ownerClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newOwnersAt(t *testing.T) (*ClusterOwners, *ownerClock) {
	t.Helper()
	c := &ownerClock{now: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	o := NewClusterOwners()
	o.now = c.get
	return o, c
}

func TestOwner_ClaimElectsTheLowestLiveName(t *testing.T) {
	o, _ := newOwnersAt(t)
	if owner, ok := o.Claim("k", "gamma", time.Minute); owner != "gamma" || !ok {
		t.Fatalf("lone claim = %q %v, want gamma owning", owner, ok)
	}
	if owner, ok := o.Claim("k", "beta", time.Minute); owner != "beta" || !ok {
		t.Fatalf("lower claim = %q %v, want beta owning", owner, ok)
	}
	if owner, ok := o.Claim("k", "gamma", time.Minute); owner != "beta" || ok {
		t.Fatalf("gamma again = %q %v, want beta, not gamma", owner, ok)
	}
	if got := o.Members("k"); !slices.Equal(got, []string{"beta", "gamma"}) {
		t.Fatalf("members = %v", got)
	}
}

// Concurrency: eight runtimes claiming at once agree on one owner.
func TestOwner_EightConcurrentClaimsElectOneOwner(t *testing.T) {
	o := NewClusterOwners()
	var wg sync.WaitGroup
	owners := make([]string, 8)
	oks := make([]bool, 8)
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for range 50 { // keep claiming while the others join
				owners[i], oks[i] = o.Claim("k", fmt.Sprintf("db%d", 7-i), time.Hour)
			}
		}(i)
	}
	wg.Wait()
	n := 0
	for i := range 8 {
		owner, ok := o.Claim("k", fmt.Sprintf("db%d", 7-i), time.Hour)
		if owner != "db0" {
			t.Fatalf("db%d sees owner %q, want db0", 7-i, owner)
		}
		if ok {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d owners, want exactly 1", n)
	}
}

// State transition: a missed heartbeat (past its ttl) or a leave
// re-elects; a returning lower name takes the cluster back.
func TestOwner_MissedHeartbeatAndLeaveReelect(t *testing.T) {
	o, clock := newOwnersAt(t)
	o.Claim("k", "alpha", 2*time.Minute)
	o.Claim("k", "beta", 2*time.Minute)
	clock.add(2 * time.Minute) // exactly the ttl: alpha is still live
	if owner, _ := o.Claim("k", "beta", 2*time.Minute); owner != "alpha" {
		t.Fatalf("at the ttl owner = %q, want alpha still", owner)
	}
	clock.add(time.Second)
	if owner, ok := o.Claim("k", "beta", 2*time.Minute); owner != "beta" || !ok {
		t.Fatalf("after alpha missed its ttl owner = %q %v, want beta", owner, ok)
	}
	o.Claim("k", "alpha", 2*time.Minute)
	if owner, _ := o.Claim("k", "beta", 2*time.Minute); owner != "alpha" {
		t.Fatalf("alpha back: owner = %q", owner)
	}
	o.Leave("k", "alpha")
	o.Leave("k", "alpha") // twice is safe
	if owner, ok := o.Claim("k", "beta", 2*time.Minute); owner != "beta" || !ok {
		t.Fatalf("after alpha left owner = %q %v", owner, ok)
	}
	o.Leave("k", "beta")
	if got := o.Members("k"); len(got) != 0 || len(o.clusters) != 0 {
		t.Fatalf("members %v clusters %d after everyone left", got, len(o.clusters))
	}
}

// A non-owner resolves its copies only after the current owner reported
// a successful evaluation inside the window; never on a former owner's.
func TestOwner_OwnerEvaluatedNeedsTheCurrentOwnersRecentReport(t *testing.T) {
	o, clock := newOwnersAt(t)
	o.Claim("k", "alpha", time.Minute)
	o.Claim("k", "beta", time.Minute)
	if o.OwnerEvaluated("k", "alpha", time.Minute) {
		t.Fatal("alpha has not reported yet")
	}
	o.Evaluated("k", "beta") // not the owner: records nothing
	if o.OwnerEvaluated("k", "beta", time.Minute) || o.OwnerEvaluated("k", "alpha",
		time.Minute) {
		t.Fatal("a non-owner's report counted")
	}
	o.Evaluated("k", "alpha")
	if !o.OwnerEvaluated("k", "alpha", time.Minute) {
		t.Fatal("alpha's report did not count")
	}
	clock.add(time.Minute + time.Second)
	o.Claim("k", "alpha", time.Hour)
	if o.OwnerEvaluated("k", "alpha", time.Minute) {
		t.Fatal("a report older than the window counted")
	}
	o.Evaluated("k", "alpha")
	o.Leave("k", "alpha")
	o.Claim("k", "beta", time.Minute)
	if o.OwnerEvaluated("k", "beta", time.Minute) || o.OwnerEvaluated("k", "alpha",
		time.Minute) {
		t.Fatal("the former owner's report counted for the new owner")
	}
}

// State transition: an evaluation recorded while not the owner, or by an
// owner that left or missed its ttl and came back, never counts until the
// owner evaluates again in its new term.
func TestOwner_AReturningOwnerCountsOnlyAfterEvaluatingAgain(t *testing.T) {
	o, clock := newOwnersAt(t)
	o.Claim("k", "alpha", time.Minute)
	o.Claim("k", "beta", time.Minute)
	o.Evaluated("k", "beta") // a non-owner: nothing recorded
	o.Leave("k", "alpha")
	if owner, ok := o.Claim("k", "beta", time.Minute); owner != "beta" || !ok {
		t.Fatalf("after alpha left owner = %q %v", owner, ok)
	}
	if o.OwnerEvaluated("k", "beta", time.Hour) {
		t.Fatal("beta's report from before it owned counted")
	}
	o.Claim("k", "alpha", time.Minute)
	o.Evaluated("k", "alpha")
	o.Leave("k", "alpha")
	o.Claim("k", "alpha", time.Minute) // a restarted runtime rejoins at once
	if o.OwnerEvaluated("k", "alpha", time.Hour) {
		t.Fatal("alpha's report from before it left counted for its new term")
	}
	o.Evaluated("k", "alpha")
	if !o.OwnerEvaluated("k", "alpha", time.Hour) {
		t.Fatal("alpha's report in its new term did not count")
	}
	clock.add(time.Minute + time.Second) // alpha missed its ttl, nobody noticed
	o.Claim("k", "alpha", time.Minute)
	if o.OwnerEvaluated("k", "alpha", time.Hour) {
		t.Fatal("alpha's report from before it expired counted")
	}
}

// Boundary and nil: an empty key or nil set is per-database; a restored
// clone (same system id, another start) is its own cluster; the role is
// not part of the key.
func TestOwner_KeysAndPerDatabaseFallbacks(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	w := probes.WALRunway{SystemID: "7301", StartedAt: start, RoleName: "sage"}
	clone := w
	clone.StartedAt = start.Add(time.Hour)
	other := w
	other.RoleName = "monitor"
	if OwnerKey(w) == "" || OwnerKey(w) == OwnerKey(clone) || OwnerKey(w) != OwnerKey(other) {
		t.Fatalf("keys %q %q %q", OwnerKey(w), OwnerKey(clone), OwnerKey(other))
	}
	if k := OwnerKey(probes.WALRunway{SystemID: "  ", StartedAt: start}); k != "" {
		t.Fatalf("blank system id key = %q", k)
	}
	o := NewClusterOwners()
	a, _ := o.Claim(OwnerKey(w), "alpha", time.Hour)
	b, ok := o.Claim(OwnerKey(clone), "beta", time.Hour)
	if a != "alpha" || b != "beta" || !ok {
		t.Fatalf("clone owners %q %q %v, want two owners", a, b, ok)
	}
	if owner, ok := o.Claim("", "gamma", time.Hour); owner != "gamma" || !ok {
		t.Fatalf("empty key = %q %v", owner, ok)
	}
	var none *ClusterOwners
	if owner, ok := none.Claim("k", "gamma", time.Hour); owner != "gamma" || !ok ||
		none.Members("k") != nil || none.OwnerEvaluated("k", "gamma", time.Hour) {
		t.Fatalf("nil set = %q %v", owner, ok)
	}
	none.Leave("k", "gamma")
	none.Evaluated("k", "gamma")
	var member *ClusterMember
	if owner, ok := member.IsClusterOwner(); owner != "" || !ok || member.OwnerHealthy("x") {
		t.Fatalf("nil member = %q %v", owner, ok)
	}
}

func scopeRunways() []Runway {
	mk := func(cat, typ, ident string) Runway {
		return Runway{Category: cat, ObjectType: typ, Identifier: ident,
			SecondsToLimit: math.NaN(), Detail: map[string]any{"subject": ident}}
	}
	return []Runway{mk(CategoryWAL, "replication_slot", "slot:fill"),
		mk(CategoryWAL, "database", "disk"), mk(CategoryWraparound, "database", "xid"),
		mk(CategoryWraparound, "table", "public.t"), mk(CategorySequence, "sequence", "s")}
}

func identsOf(rs []Runway) string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Identifier)
	}
	return strings.Join(out, ",")
}

func TestOwner_NonOwnerScopeKeepsOnlyItsDatabasesRunways(t *testing.T) {
	s := clusterScope{key: "k", owner: "alpha", shared: true}
	held := map[string]map[string]string{CategoryWAL: {"disk": HoldUnmeasured},
		CategoryWraparound: {"mxid": HoldNoSamples}, CategorySequence: {"s2": HoldUnmeasured}}
	rs, cats, kept := s.apply(scopeRunways(), []string{CategoryWraparound, CategoryWAL,
		CategorySequence}, held)
	if identsOf(rs) != "public.t,s" {
		t.Fatalf("non-owner runways = %s, want the table and the sequence", identsOf(rs))
	}
	if !slices.Equal(cats, []string{CategoryWraparound, CategorySequence}) {
		t.Fatalf("non-owner evaluated %v, want WAL left to the owner", cats)
	}
	if len(kept) != 1 || kept[CategorySequence]["s2"] != HoldUnmeasured {
		t.Fatalf("non-owner holds %v, want the sequence only", kept)
	}
	keep := map[string]bool{}
	s.keepCluster(CategoryWraparound, keep)
	if !keep["xid"] || !keep["mxid"] || len(keep) != 2 {
		t.Fatalf("wraparound keep = %v, want xid and mxid", keep)
	}
	seq := map[string]bool{}
	s.keepCluster(CategorySequence, seq)
	if len(seq) != 0 {
		t.Fatalf("sequence keep = %v", seq)
	}
	if _, cats, kept := s.apply(nil, []string{CategoryWAL}, nil); len(cats) != 0 ||
		kept != nil {
		t.Fatalf("unreadable trends: evaluated %v holds %v", cats, kept)
	}
}

func TestOwner_OwnerScopeAnnotatesOnlyClusterFindings(t *testing.T) {
	s := clusterScope{key: "k", owner: "alpha", shared: true, isOwner: true,
		members: []string{"alpha", "beta", "gamma"}}
	in := scopeRunways()
	rs, cats, _ := s.apply(in, []string{CategoryWAL}, nil)
	if len(rs) != 5 || !slices.Equal(cats, []string{CategoryWAL}) {
		t.Fatalf("owner runways %s evaluated %v", identsOf(rs), cats)
	}
	for _, r := range rs {
		cluster := isClusterRunway(r.Category, r.Identifier)
		if got := r.Detail["scope"] == ScopeCluster; got != cluster {
			t.Fatalf("%s scope %v, want cluster=%v", r.Identifier, r.Detail["scope"], cluster)
		}
		if cluster && (r.Detail["owner_database"] != "alpha" ||
			!slices.Equal(r.Detail["cluster_databases"].([]string), s.members) ||
			r.Detail["subject"] != r.Identifier) {
			t.Fatalf("%s detail = %v", r.Identifier, r.Detail)
		}
	}
	if in[0].Detail["scope"] != nil {
		t.Fatal("annotating changed the caller's detail map")
	}
	keep := map[string]bool{}
	s.keepCluster(CategoryWraparound, keep)
	if len(keep) != 0 {
		t.Fatalf("owner keep = %v, want nothing forced open", keep)
	}
	solo := clusterScope{}
	if rs, _, _ := solo.apply(in, nil, nil); len(rs) != 5 || rs[0].Detail["scope"] != nil {
		t.Fatalf("unshared scope changed the runways: %s", identsOf(rs))
	}
}

// claimCluster: a standby or an unreadable system id holds no claim (the
// latter is logged once), a failed WAL reading keeps the last key and a
// new key leaves the old one.
func TestOwner_ClaimClusterFollowsTheWALReading(t *testing.T) {
	o := NewClusterOwners()
	var logs []string
	m := &Monitor{opts: Options{Database: "beta", Interval: time.Minute, Owners: o},
		logFn: func(_ string, msg string, _ ...any) { logs = append(logs, msg) }}
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	w := &probes.WALRunway{SystemID: "7301", StartedAt: start}
	o.Claim(OwnerKey(*w), "alpha", time.Hour)
	s := m.claimCluster(Snapshot{WAL: w})
	if !s.shared || s.isOwner || s.owner != "alpha" || s.reports() {
		t.Fatalf("scope = %+v, want alpha owning", s)
	}
	if s := m.claimCluster(Snapshot{}); !s.shared || s.key != OwnerKey(*w) {
		t.Fatalf("failed WAL reading scope = %+v, want the last key", s)
	}
	restarted := &probes.WALRunway{SystemID: "7301", StartedAt: start.Add(time.Hour)}
	if s := m.claimCluster(Snapshot{WAL: restarted}); !s.isOwner ||
		slices.Contains(o.Members(OwnerKey(*w)), "beta") {
		t.Fatalf("restart scope = %+v, old members %v", s, o.Members(OwnerKey(*w)))
	}
	for range 2 {
		if s := m.claimCluster(Snapshot{WAL: &probes.WALRunway{}}); s.shared ||
			!s.reports() {
			t.Fatalf("no system id scope = %+v, want per-database", s)
		}
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "system identifier is unreadable") {
		t.Fatalf("logs = %q, want one unreadable-identifier line", logs)
	}
	m.claimCluster(Snapshot{WAL: restarted})
	standby := *restarted
	standby.InRecovery = true
	if s := m.claimCluster(Snapshot{WAL: &standby}); s.shared ||
		len(o.Members(OwnerKey(*restarted))) != 0 {
		t.Fatalf("standby scope %+v kept its claim", s)
	}
	m.claimCluster(Snapshot{WAL: restarted})
	m.leaveCluster()
	if len(o.Members(OwnerKey(*restarted))) != 0 {
		t.Fatal("leaveCluster kept the claim")
	}
}
