package causal

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Checkpoint storm family: requested checkpoints driven by WAL volume
// (max_wal_size too small), forced checkpoints (CHECKPOINT commands or
// backup tools), a short checkpoint_timeout, and a write burst that only
// amplifies them. Compared across the first and last checkpoint_activity
// samples of one statistics epoch.

var ckptReset = m6t0.Add(-24 * time.Hour)

type ckpt struct {
	timed, req     int64
	wal            float64
	maxWAL         float64
	timeout        int64
	reset, started time.Time
	backendFsync   int64
}

func baseCkpt() ckpt {
	return ckpt{timed: 50, req: 10, wal: 10 << 30, maxWAL: 1 << 30, timeout: 300,
		reset: ckptReset, started: ckptReset}
}

func (c ckpt) row() probes.Row {
	return probes.Row{"timed_checkpoints": c.timed, "requested_checkpoints": c.req,
		"checkpoint_write_ms": 1000.0, "checkpoint_sync_ms": 10.0,
		"buffers_written": int64(5000), "backend_writes": int64(0),
		"backend_fsyncs": c.backendFsync, "checkpointer_stats_reset": c.reset,
		"wal_bytes": c.wal, "wal_records": int64(1000), "wal_fpi": int64(100),
		"wal_stats_reset": nil, "max_wal_size_bytes": c.maxWAL,
		"checkpoint_timeout_s": c.timeout, "checkpoint_completion_target": 0.9,
		"server_started_at": c.started}
}

func ckptObs(ev string, at time.Duration, c ckpt) Observation {
	return obsAt(ev, probes.CheckpointActivity, at, c.row())
}

// window is three samples, 18 s apart end to end; b changes the last.
func window(a ckpt, b func(*ckpt)) []Observation {
	mid, last := a, a
	b(&last)
	mid.req, mid.timed, mid.wal = (a.req+last.req)/2, (a.timed+last.timed)/2,
		(a.wal+last.wal)/2
	return []Observation{ckptObs("C1", 0, a), ckptObs("C2", 9*time.Second, mid),
		ckptObs("C3", 18*time.Second, last)}
}

func TestCheckpoint_UndersizedMaxWALSizeUnderABurst(t *testing.T) {
	a := baseCkpt()
	a.maxWAL = 32 * mib
	d := DiagnoseCheckpoint(window(a, func(c *ckpt) { c.req += 5; c.wal += 128 * mib }))
	if d.Family != FamilyCheckpoint || d.GraphVersion != GraphVersion {
		t.Fatalf("family/version = %s/%s", d.Family, d.GraphVersion)
	}
	wantRoot(t, d, MaxWALSizeUndersized)
	wantStatus(t, d, CheckpointWriteBurst, StatusContributing)
	wantStatus(t, d, ForcedCheckpoints, StatusRuledOut)
	wantStatus(t, d, ShortCheckpointTimeout, StatusRuledOut)
	wantFact(t, d.Root.Support, "C3", "5 checkpoints were requested in 18 s")
	wantFact(t, d.Root.Support, "C3", "max_wal_size")
	if len(d.Observed) == 0 {
		t.Fatal("the WAL rate is not stated as an observed fact")
	}
	wantFact(t, d.Observed, "C3", "bytes/s")
	everyHypothesisCarriesRefutation(t, d)
}

func TestCheckpoint_ForcedCheckpointsWithLittleWAL(t *testing.T) {
	d := DiagnoseCheckpoint(window(baseCkpt(), func(c *ckpt) { c.req += 12; c.wal += 64 << 10 }))
	wantRoot(t, d, ForcedCheckpoints)
	wantStatus(t, d, MaxWALSizeUndersized, StatusRuledOut)
	wantStatus(t, d, CheckpointWriteBurst, StatusRuledOut)
	wantFact(t, d.Root.Support, "C3", "12 checkpoints were requested in 18 s")
	h, _ := hypothesisOf(d, MaxWALSizeUndersized)
	wantFact(t, h.Contradict, "C3", "under a quarter of max_wal_size")
}

// Decoy: a burst that fits within max_wal_size requests no checkpoint.
func TestCheckpoint_BurstWithinMaxWALSizeIsInconclusive(t *testing.T) {
	d := DiagnoseCheckpoint(window(baseCkpt(), func(c *ckpt) { c.wal += 96 * mib }))
	wantInconclusive(t, d)
	wantStatus(t, d, CheckpointWriteBurst, StatusRuledOut)
	wantStatus(t, d, ForcedCheckpoints, StatusRuledOut)
	wantStatus(t, d, MaxWALSizeUndersized, StatusAlternative)
	h, _ := hypothesisOf(d, CheckpointWriteBurst)
	wantFact(t, h.Contradict, "C3", "no checkpoint was requested")
}

func TestCheckpoint_QuietServerRulesEverythingOut(t *testing.T) {
	d := DiagnoseCheckpoint(window(baseCkpt(), func(c *ckpt) { c.wal += 4096 }))
	wantInconclusive(t, d)
	for _, id := range []NodeID{MaxWALSizeUndersized, ForcedCheckpoints,
		ShortCheckpointTimeout, CheckpointWriteBurst} {
		wantStatus(t, d, id, StatusRuledOut)
	}
}

func TestCheckpoint_ShortCheckpointTimeout(t *testing.T) {
	a := baseCkpt()
	a.timeout = 30
	d := DiagnoseCheckpoint(window(a, func(c *ckpt) { c.timed += 2; c.wal += mib }))
	wantRoot(t, d, ShortCheckpointTimeout)
	wantFact(t, d.Root.Support, "C3", "checkpoint_timeout")
}

// Boundary: WAL per requested checkpoint at exactly a quarter of
// max_wal_size is volume-driven; a byte less is forced.
func TestCheckpoint_VolumeShareBoundary(t *testing.T) {
	a := baseCkpt()
	a.maxWAL = 64 * mib
	at := DiagnoseCheckpoint(window(a, func(c *ckpt) { c.req += 2; c.wal += 32 * mib }))
	if statusOf(at, MaxWALSizeUndersized) == StatusRuledOut ||
		statusOf(at, ForcedCheckpoints) != StatusRuledOut {
		t.Fatalf("at the boundary: undersized %s forced %s", statusOf(at,
			MaxWALSizeUndersized), statusOf(at, ForcedCheckpoints))
	}
	below := DiagnoseCheckpoint(window(a, func(c *ckpt) { c.req += 2; c.wal += 32*mib - 2 }))
	wantStatus(t, below, MaxWALSizeUndersized, StatusRuledOut)
	wantRoot(t, below, ForcedCheckpoints)
}

// CHECK-07: a statistics reset, a falling counter or a restart between
// samples make the deltas unknown; nothing is contradicted by them.
func TestCheckpoint_CounterResetMakesDeltasUnknown(t *testing.T) {
	cases := map[string]func(*ckpt){
		"stats reset":     func(c *ckpt) { c.reset = m6t0; c.req = 1; c.wal += 64 * mib },
		"falling counter": func(c *ckpt) { c.req -= 5; c.wal += 64 * mib },
		"server restart":  func(c *ckpt) { c.started = m6t0; c.req += 9 },
	}
	for name, mutate := range cases {
		d := DiagnoseCheckpoint(window(baseCkpt(), mutate))
		wantInconclusive(t, d)
		wantMissing(t, d, probes.CheckpointActivity, "counter_reset")
		for _, id := range []NodeID{MaxWALSizeUndersized, ForcedCheckpoints,
			CheckpointWriteBurst} {
			if statusOf(d, id) == StatusRuledOut {
				t.Errorf("%s: %s ruled out from incomparable samples", name, id)
			}
		}
	}
}

func TestCheckpoint_MissingAndUnavailableEvidence(t *testing.T) {
	none := DiagnoseCheckpoint(nil)
	wantInconclusive(t, none)
	wantMissing(t, none, probes.CheckpointActivity, "not_collected")
	one := DiagnoseCheckpoint([]Observation{ckptObs("C1", 0, baseCkpt())})
	wantInconclusive(t, one)
	wantMissing(t, one, probes.CheckpointActivity, "second_sample_unavailable")
	denied := DiagnoseCheckpoint([]Observation{failedObs("C1", probes.CheckpointActivity,
		probes.StatusNoPrivilege, "insufficient_privilege"), failedObs("C2",
		probes.CheckpointActivity, probes.StatusNoPrivilege, "insufficient_privilege")})
	wantInconclusive(t, denied)
	wantMissing(t, denied, probes.CheckpointActivity, "insufficient_privilege")
}

func TestCheckpoint_UnknownMaxWALSizeCannotClassifyRequests(t *testing.T) {
	a := baseCkpt()
	a.maxWAL = nanValue()
	d := DiagnoseCheckpoint(window(a, func(c *ckpt) { c.req += 4; c.wal += 64 * mib }))
	wantInconclusive(t, d)
	for _, id := range []NodeID{MaxWALSizeUndersized, ForcedCheckpoints} {
		if statusOf(d, id) != StatusAlternative {
			t.Errorf("%s = %s, want unproven without max_wal_size", id, statusOf(d, id))
		}
	}
}

func TestCheckpoint_BackendFsyncsAreObserved(t *testing.T) {
	a := baseCkpt()
	a.maxWAL = 32 * mib
	d := DiagnoseCheckpoint(window(a, func(c *ckpt) {
		c.req += 4
		c.wal += 96 * mib
		c.backendFsync += 37
	}))
	wantFact(t, d.Observed, "C3", "37")
}
