package rca

import (
	"crypto/md5"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/sanitize"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// rca-01: a prepared transaction is blamed (and signalled as orphaned)
// only when older than rca.prepared_xact_min_age_minutes, and its GID is
// quoted as a literal in ROLLBACK PREPARED.

// No concurrent access test: the detector and the tree read one snapshot.

// md5Hex is the md5 of gid, as the xmin_horizon query names a prepared
// transaction.
func md5Hex(gid string) string {
	sum := md5.Sum([]byte(gid))
	return hex.EncodeToString(sum[:])
}

func preparedHolder(gid string, xidAge float64, age time.Duration) probes.XminHolder {
	return probes.XminHolder{Kind: probes.HolderPreparedXact, Name: md5Hex(gid),
		State: "prepared", XminAge: xidAge, XactAgeS: age.Seconds()}
}

func preparedXact(gid string, xidAge int64, age time.Duration) collector.PreparedTransaction {
	return collector.PreparedTransaction{GID: gid, Prepared: holderNow.Add(-age),
		Owner: "app", Database: "orders", XIDAge: xidAge}
}

// Invalid input: a GID with a quote, a semicolon and a newline is one
// correctly quoted literal, never statement text.
func TestPreparedXact_HostileGIDIsQuoted(t *testing.T) {
	gid := "tx'; DROP TABLE orders; --\nx"
	snap := holderSnapshot([]probes.XminHolder{preparedHolder(gid, 70000, 2*time.Hour)})
	snap.PreparedXacts = []collector.PreparedTransaction{preparedXact(gid, 70000, 2*time.Hour)}
	inc := newTestEngine().treeVacuumBlocked(holderSignal(), snap, testVacuumRules())
	want := "ROLLBACK PREPARED 'tx''; DROP TABLE orders; --\nx';"
	if inc.RecommendedSQL != want || want != "ROLLBACK PREPARED "+sanitize.QuoteLiteral(gid)+";" {
		t.Fatalf("sql = %q, want %q", inc.RecommendedSQL, want)
	}
	if inc.ActionRisk != "high_risk" || inc.Severity != "critical" {
		t.Fatalf("risk %q severity %q, want high_risk and critical", inc.ActionRisk,
			inc.Severity)
	}
	b := linkBlocker(t, inc)
	if b == nil || b.HolderKind != "prepared_xact" || b.HolderName != md5Hex(gid) ||
		b.XminAge != 70000 || b.PID != 0 {
		t.Fatalf("blocker = %+v, want the prepared transaction by gid hash", b)
	}
}

// Boundary: the prepared-transaction floor is inclusive, through the
// holder read and through the collector's list alone.
func TestPreparedXact_FloorBoundary(t *testing.T) {
	for _, tc := range []struct {
		age   time.Duration
		named bool
	}{{30*time.Minute - time.Second, false}, {30 * time.Minute, true},
		{30*time.Minute + time.Second, true}} {
		for _, viaHolders := range []bool{true, false} {
			snap := holderSnapshot(nil)
			if viaHolders {
				snap.XminHolders = []probes.XminHolder{preparedHolder("g1", 6000, tc.age)}
			}
			snap.PreparedXacts = []collector.PreparedTransaction{preparedXact("g1", 6000, tc.age)}
			inc := newTestEngine().treeVacuumBlocked(holderSignal(), snap, testVacuumRules())
			named := strings.Contains(inc.RecommendedSQL, "ROLLBACK PREPARED 'g1';")
			if named != tc.named {
				t.Errorf("age %s via holders %v: sql %q, named = %v, want %v", tc.age,
					viaHolders, inc.RecommendedSQL, named, tc.named)
			}
			if !tc.named {
				assertBranch3(t, inc)
			}
		}
	}
}

// A prepared holder whose GID the collector did not list is named by its
// hash, with no ROLLBACK (there is no GID to quote).
func TestPreparedXact_UnlistedGIDGetsNoRollback(t *testing.T) {
	inc := blockedHolder(t, []probes.XminHolder{preparedHolder("lost", 6000, time.Hour)})
	if !strings.Contains(inc.RootCause, md5Hex("lost")) {
		t.Fatalf("root cause %q, want the gid hash", inc.RootCause)
	}
	assertNoSignal(t, inc)
	if !strings.Contains(inc.RecommendedSQL, "pg_prepared_xacts") {
		t.Fatalf("diagnostic %q, want a pg_prepared_xacts read", inc.RecommendedSQL)
	}
}

// CHECK-03: with defaults no orphaned_prepared_tx signal fires for a
// prepared transaction younger than 30 minutes; at 30 it does, and the
// oldest aged one is reported.
func TestDetectOrphanedPreparedTx_AgeFloor(t *testing.T) {
	eng := newTestEngine()
	snap := &collector.Snapshot{CollectedAt: holderNow,
		PreparedXacts: []collector.PreparedTransaction{
			preparedXact("young", 900_000, 30*time.Minute-time.Second),
		}}
	if sig := eng.detectOrphanedPreparedTx(snap); sig != nil {
		t.Fatalf("signal %+v for a prepared xact 1 s under the floor, want none", sig.Metrics)
	}
	snap.PreparedXacts = append(snap.PreparedXacts, preparedXact("aged", 1000, 30*time.Minute))
	sig := eng.detectOrphanedPreparedTx(snap)
	if sig == nil {
		t.Fatal("no signal for a prepared xact at the floor")
	}
	if g := stringMetric(sig, "oldest_gid"); g != "aged" || intMetric(sig, "max_xid_age") != 1000 ||
		intMetric(sig, "count") != 2 {
		t.Fatalf("metrics = %+v, want oldest aged gid 'aged' (xid age 1000) of 2", sig.Metrics)
	}
	incs := eng.Analyze(&collector.Snapshot{CollectedAt: time.Now(),
		System: collector.SystemStats{MaxConnections: 100, CacheHitRatio: 0.999},
		PreparedXacts: []collector.PreparedTransaction{{GID: "fresh",
			Prepared: time.Now().Add(-29 * time.Minute), XIDAge: 50}}}, nil, testConfig(), nil)
	for _, inc := range incs {
		for _, id := range inc.SignalIDs {
			if id == "orphaned_prepared_tx" {
				t.Fatalf("defaults opened %q for a 29-minute prepared xact", inc.RootCause)
			}
		}
	}
}

// Invalid input: a GID with a backslash is quoted so that it reads the
// same with standard_conforming_strings off (an escape string), and a
// quote after the backslash cannot end the literal.
func TestPreparedXact_BackslashGIDIsAnEscapeString(t *testing.T) {
	gid := `x\'; DROP TABLE orders; --`
	snap := holderSnapshot([]probes.XminHolder{preparedHolder(gid, 70000, 2*time.Hour)})
	snap.PreparedXacts = []collector.PreparedTransaction{preparedXact(gid, 70000, 2*time.Hour)}
	inc := newTestEngine().treeVacuumBlocked(holderSignal(), snap, testVacuumRules())
	want := `ROLLBACK PREPARED E'x\\''; DROP TABLE orders; --';`
	if inc.RecommendedSQL != want {
		t.Fatalf("sql = %q, want %q", inc.RecommendedSQL, want)
	}
	if got := gidLiteral("plain'gid"); got != "'plain''gid'" || strings.HasPrefix(got, "E") {
		t.Fatalf("gidLiteral(plain'gid) = %q, want a standard literal", got)
	}
}
