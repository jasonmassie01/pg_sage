package tuner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/advisor"
)

// tuning-03: a stat_temp_spill hint is sized from the interval between two
// Tune cycles (per call), against the work_mem the statement's role runs
// with, and capped by min(tuner.work_mem_max_mb, 5% of host memory).

const (
	appUser   int64 = 16384
	otherUser int64 = 16385
	fourMB    int64 = 4096 // kB
)

// fakeSpillReader stands in for the per-cycle read of block_size and the
// roles' work_mem.
type fakeSpillReader struct {
	mu       sync.Mutex
	settings spillSettings
	err      error
	reads    int
	users    [][]int64
}

func (f *fakeSpillReader) read(_ context.Context, users []int64) (spillSettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	f.users = append(f.users, append([]int64(nil), users...))
	return f.settings, f.err
}

func newSpillReader(workMemKB int64) *fakeSpillReader {
	return &fakeSpillReader{settings: spillSettings{BlockSize: 8192,
		WorkMemKB: map[int64]int64{appUser: workMemKB}}}
}

func hostOf(bytes int64) advisor.HostMemorySource {
	return func() advisor.HostMemory { return advisor.HostMemory{TotalBytes: bytes} }
}

// spillTuner is a tuner without a database whose stat-spill settings come
// from r; hostBytes 0 leaves host memory unknown.
func spillTuner(r *fakeSpillReader, maxMB int, hostBytes int64) *Tuner {
	tu := New(nil, TunerConfig{WorkMemMaxMB: maxMB}, &HintPlanAvailability{ApplicationEffect: true,
		HintTableReady: true}, noopLogFn)
	tu.spill.read = r.read
	if hostBytes > 0 {
		tu.WithHostMemory(hostOf(hostBytes))
	}
	return tu
}

func spillCand(qid, user, calls, blks int64) candidate {
	return candidate{QueryID: qid, UserID: user, Query: "SELECT payload FROM t ORDER BY 1",
		Calls: calls, TempBlksWritten: blks}
}

// spillHint observes each cycle's candidate in turn and returns the hint the
// deterministic rules prescribe for the last one.
func spillHint(t *testing.T, tu *Tuner, cycles ...candidate) string {
	t.Helper()
	ctx := context.Background()
	for _, c := range cycles {
		tu.observeSpills(ctx, []candidate{c})
	}
	last := cycles[len(cycles)-1]
	return CombineHints(tu.prescribeAll(tu.gatherSymptoms(ctx, last)))
}

func TestStatSpill_OneMBPerCallAtFourMBGivesNoHint(t *testing.T) {
	// 100k calls at 128 blocks (1 MB) each: the per-call spill is below the
	// role's 4 MB work_mem. Sizing the 12.8M-block cumulative total as one
	// execution's spill used to prescribe the 512 MB ceiling.
	tu := spillTuner(newSpillReader(fourMB), 512, 16<<30)
	got := spillHint(t, tu, spillCand(7, appUser, 500, 64_000),
		spillCand(7, appUser, 100_500, 64_000+100_000*128))
	if got != "" {
		t.Fatalf("hint = %q, want none: 1 MB per call fits a 4 MB work_mem", got)
	}
}

func TestStatSpill_SixteenMBPerCallGivesExactly32MB(t *testing.T) {
	tu := spillTuner(newSpillReader(fourMB), 512, 16<<30)
	got := spillHint(t, tu, spillCand(7, appUser, 10, 0),
		spillCand(7, appUser, 1_010, 1_000*2_048))
	if got != `Set(work_mem "32MB")` {
		t.Fatalf("hint = %q, want Set(work_mem \"32MB\"): 4 doubled until >= 2 x 16 MB", got)
	}
}

func TestStatSpill_FirstSightingAndZeroCallsGiveNoSymptom(t *testing.T) {
	r := newSpillReader(fourMB)
	tu := spillTuner(r, 512, 16<<30)
	if got := spillHint(t, tu, spillCand(7, appUser, 1_000, 1_000*2_048)); got != "" {
		t.Fatalf("first sighting: hint = %q, want none (it only records the baseline)", got)
	}
	if got := spillHint(t, tu, spillCand(7, appUser, 1_000, 2_000*2_048)); got != "" {
		t.Fatalf("no new calls: hint = %q, want none", got)
	}
	if got := spillHint(t, tu, spillCand(7, appUser, 1_500, 2_000*2_048)); got != "" {
		t.Fatalf("calls without new temp blocks: hint = %q, want none", got)
	}
	if r.reads != 0 {
		t.Fatalf("settings read %d times: without a positive delta nothing is sized", r.reads)
	}
	if tu.spill.baselines[spillKey{7, appUser}].Calls != 1_500 {
		t.Fatalf("baseline = %+v, want the latest counters", tu.spill.baselines)
	}
}

func TestStatSpill_StatsResetStartsANewBaseline(t *testing.T) {
	tu := spillTuner(newSpillReader(fourMB), 512, 16<<30)
	got := spillHint(t, tu, spillCand(7, appUser, 5_000, 9_000_000),
		spillCand(7, appUser, 40, 40*2_048)) // pg_stat_statements_reset()
	if got != "" {
		t.Fatalf("negative delta: hint = %q, want none", got)
	}
	b := tu.spill.baselines[spillKey{7, appUser}]
	if b.Calls != 40 || b.Blocks != 40*2_048 {
		t.Fatalf("baseline after reset = %+v, want the post-reset counters", b)
	}
	if got := spillHint(t, tu, spillCand(7, appUser, 1_040, 40*2_048+1_000*2_048)); got !=
		`Set(work_mem "32MB")` {
		t.Fatalf("the interval after the reset is sized from the new baseline: %q", got)
	}
}

func TestStatSpill_PerCallSpillEqualToBaselineGivesNoHint(t *testing.T) {
	// 512 blocks of 8 kB = 4096 kB per call, exactly the role's work_mem.
	tu := spillTuner(newSpillReader(fourMB), 512, 16<<30)
	if got := spillHint(t, tu, spillCand(7, appUser, 10, 0),
		spillCand(7, appUser, 20, 10*512)); got != "" {
		t.Fatalf("hint = %q, want none at the boundary", got)
	}
	// One more block per call is above it.
	if got := spillHint(t, tu, spillCand(7, appUser, 30, 10*512+10*513)); got !=
		`Set(work_mem "16MB")` {
		t.Fatalf("hint = %q, want 16MB just above the boundary", got)
	}
}

func TestStatSpill_NeedAboveCeilingClampsToExactlyTheCeiling(t *testing.T) {
	// 1 GB per call needs 2 GB; tuner.work_mem_max_mb is 300 and the host
	// (16 GiB, cap 819 MB) is not the binding bound.
	tu := spillTuner(newSpillReader(fourMB), 300, 16<<30)
	got := spillHint(t, tu, spillCand(7, appUser, 10, 0),
		spillCand(7, appUser, 20, 10*131_072))
	if got != `Set(work_mem "300MB")` {
		t.Fatalf("hint = %q, want exactly the 300MB ceiling", got)
	}
	// On a 4 GiB host the 5% cap (204 MB) binds below the configured 512.
	tu = spillTuner(newSpillReader(fourMB), 512, 4<<30)
	got = spillHint(t, tu, spillCand(7, appUser, 10, 0),
		spillCand(7, appUser, 20, 10*131_072))
	if got != `Set(work_mem "204MB")` {
		t.Fatalf("hint = %q, want the 204MB host cap (5%% of 4 GiB)", got)
	}
}

func TestStatSpill_CeilingAtTheBaselineGivesNoNoOpSet(t *testing.T) {
	// The role already runs with 300 MB; the ceiling is 300 MB, so the
	// clamped size equals the baseline and would change nothing.
	tu := spillTuner(newSpillReader(300*1024), 300, 0)
	if got := spillHint(t, tu, spillCand(7, appUser, 10, 0),
		spillCand(7, appUser, 20, 10*131_072)); got != "" {
		t.Fatalf("hint = %q, want none: never a no-op Set", got)
	}
}

func TestStatSpill_UsesTheStatementRolesWorkMem(t *testing.T) {
	r := newSpillReader(fourMB)
	r.settings.WorkMemKB[otherUser] = 64 * 1024
	tu := spillTuner(r, 512, 16<<30)
	got := spillHint(t, tu, spillCand(7, otherUser, 10, 0),
		spillCand(7, otherUser, 1_010, 1_000*2_048))
	if got != "" {
		t.Fatalf("hint = %q: 16 MB per call fits the role's 64 MB work_mem", got)
	}
	if len(r.users) != 1 || len(r.users[0]) != 1 || r.users[0][0] != otherUser {
		t.Fatalf("settings read for users %v, want only the statement's role", r.users)
	}
}

func TestStatSpill_UnreadableSettingsFailClosed(t *testing.T) {
	r := newSpillReader(fourMB)
	r.err = errors.New("permission denied for table pg_db_role_setting")
	var logged []string
	tu := spillTuner(r, 512, 16<<30)
	tu.logFn = func(level, format string, args ...any) { logged = append(logged, level+format) }
	if got := spillHint(t, tu, spillCand(7, appUser, 10, 0),
		spillCand(7, appUser, 1_010, 1_000*2_048)); got != "" {
		t.Fatalf("hint = %q, want none when the role's work_mem cannot be read", got)
	}
	if len(logged) == 0 || !strings.Contains(logged[0], "work_mem") {
		t.Fatalf("logged %q: the skipped sizing is logged with its cause", logged)
	}
	// A role missing from the settings, or an unknown block size, sizes nothing.
	r.err = nil
	r.settings = spillSettings{BlockSize: 8192, WorkMemKB: map[int64]int64{}}
	if got := spillHint(t, tu, spillCand(7, appUser, 2_010, 2_000*2_048)); got != "" {
		t.Fatalf("unknown role work_mem: hint = %q", got)
	}
	r.settings = spillSettings{WorkMemKB: map[int64]int64{appUser: fourMB}}
	if got := spillHint(t, tu, spillCand(7, appUser, 3_010, 3_000*2_048)); got != "" {
		t.Fatalf("unknown block size: hint = %q", got)
	}
}

func TestStatSpill_BlockSizeFromTheServer(t *testing.T) {
	// A 16 kB-block build: 1,024 blocks per call is 16 MB, not 8 MB.
	r := newSpillReader(fourMB)
	r.settings.BlockSize = 16384
	tu := spillTuner(r, 512, 16<<30)
	if got := spillHint(t, tu, spillCand(7, appUser, 10, 0),
		spillCand(7, appUser, 1_010, 1_000*1_024)); got != `Set(work_mem "32MB")` {
		t.Fatalf("hint = %q, want 32MB from 16 MB per call", got)
	}
}

func TestStatSpill_DuplicateRowsOfOneKeyAreNotSized(t *testing.T) {
	// track = all splits a statement into top-level and nested rows with
	// one (queryid, userid): their counters cannot be diffed apart.
	tu := spillTuner(newSpillReader(fourMB), 512, 16<<30)
	ctx := context.Background()
	tu.observeSpills(ctx, []candidate{spillCand(7, appUser, 10, 0)})
	dup := []candidate{spillCand(7, appUser, 1_010, 1_000*2_048), spillCand(7, appUser, 5, 9)}
	tu.observeSpills(ctx, dup)
	if syms := tu.gatherSymptoms(ctx, dup[0]); len(syms) != 0 {
		t.Fatalf("symptoms = %+v, want none for an ambiguous key", syms)
	}
	if _, kept := tu.spill.baselines[spillKey{7, appUser}]; kept {
		t.Fatal("an ambiguous key keeps no baseline")
	}
}

func TestStatSpill_RolesAreTrackedApart(t *testing.T) {
	r := newSpillReader(fourMB)
	r.settings.WorkMemKB[otherUser] = fourMB
	tu := spillTuner(r, 512, 16<<30)
	ctx := context.Background()
	tu.observeSpills(ctx, []candidate{spillCand(7, appUser, 10, 0),
		spillCand(7, otherUser, 10, 0)})
	cur := []candidate{spillCand(7, appUser, 1_010, 1_000*2_048), spillCand(7, otherUser, 20, 0)}
	tu.observeSpills(ctx, cur)
	if got := CombineHints(tu.prescribeAll(tu.gatherSymptoms(ctx, cur[0]))); got !=
		`Set(work_mem "32MB")` {
		t.Fatalf("app role: %q", got)
	}
	if syms := tu.gatherSymptoms(ctx, cur[1]); len(syms) != 0 {
		t.Fatalf("the other role did not spill this interval: %+v", syms)
	}
}

func TestStatSpill_BaselinesAreBounded(t *testing.T) {
	tu := spillTuner(newSpillReader(fourMB), 512, 16<<30)
	ctx := context.Background()
	for i := int64(0); i < maxSpillBaselines; i++ {
		tu.spill.baselines[spillKey{i + 100, appUser}] = spillBaseline{Calls: 1, Seen: 1}
	}
	tu.spill.cycle = 5
	tu.observeSpills(ctx, []candidate{spillCand(1, appUser, 10, 0)})
	if n := len(tu.spill.baselines); n != maxSpillBaselines {
		t.Fatalf("%d baselines, want the cap %d", n, maxSpillBaselines)
	}
	if _, ok := tu.spill.baselines[spillKey{1, appUser}]; !ok {
		t.Fatal("the statement seen this cycle is kept; the stalest is evicted")
	}
}

func TestStatSpill_DetailRecordsTheArithmetic(t *testing.T) {
	tu := spillTuner(newSpillReader(fourMB), 300, 16<<30)
	ctx := context.Background()
	tu.observeSpills(ctx, []candidate{spillCand(7, appUser, 10, 0)})
	c := spillCand(7, appUser, 20, 10*131_072)
	tu.observeSpills(ctx, []candidate{c})
	syms := tu.gatherSymptoms(ctx, c)
	if len(syms) != 1 || syms[0].Kind != SymptomStatTempSpill {
		t.Fatalf("symptoms = %+v", syms)
	}
	d := syms[0].Detail
	if d["delta_calls"] != int64(10) || d["delta_temp_blks_written"] != int64(1_310_720) ||
		d["block_size"] != int64(8192) || d["per_call_spill_kb"] != int64(1_048_576) ||
		d["baseline_work_mem_kb"] != fourMB || d["needed_mb"] != 2048 ||
		d["ceiling_mb"] != 300 || d["hint_mb"] != 300 || d["clamped"] != true {
		t.Fatalf("detail = %v", d)
	}
	p := Prescribe(syms[0], tu.cfg)
	if p == nil || !strings.Contains(p.Rationale, "per call") ||
		!strings.Contains(p.Rationale, "300") {
		t.Fatalf("prescription = %+v: the rationale states the per-call size and the clamp", p)
	}
}

func TestSizeStatSpillWorkMem(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		perCallKB, baselineKB  int64
		ceiling, wantNeed, got int
		ok                     bool
	}{
		{"16MB at 4MB", 16 * 1024, fourMB, 512, 32, 32, true},
		{"no 64MB floor", 5 * 1024, fourMB, 512, 16, 16, true},
		{"equal to baseline", fourMB, fourMB, 512, 0, 0, false},
		{"below baseline", 1024, fourMB, 512, 0, 0, false},
		{"clamped", 1 << 20, fourMB, 300, 2048, 300, true},
		{"ceiling at baseline", 1 << 20, 300 * 1024, 300, 2048, 0, false},
		{"sub-MB baseline rounds up", 100, 64, 512, 1, 1, true},
		{"zero baseline", 1024, 0, 512, 0, 0, false},
		{"zero ceiling", 16 * 1024, fourMB, 0, 0, 0, false},
		{"negative spill", -5, fourMB, 512, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			need, got, ok := sizeStatSpillWorkMem(tc.perCallKB, tc.baselineKB, tc.ceiling)
			if ok != tc.ok || got != tc.got || (tc.ok && need != tc.wantNeed) {
				t.Fatalf("size(%d, %d, %d) = need %d hint %d ok %v, want %d %d %v",
					tc.perCallKB, tc.baselineKB, tc.ceiling, need, got, ok,
					tc.wantNeed, tc.got, tc.ok)
			}
		})
	}
}

func TestParseMemoryKB(t *testing.T) {
	for in, want := range map[string]int64{"4096": 4096, "64MB": 65536, "1GB": 1 << 20,
		"64kB": 64, " 64 MB ": 65536, "1.5MB": 1536, "8192B": 8, "1TB": 1 << 30} {
		if got, err := parseMemoryKB(in); err != nil || got != want {
			t.Errorf("parseMemoryKB(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "MB", "-4MB", "0", "4mb", "4 XB", "abc"} {
		if got, err := parseMemoryKB(bad); err == nil {
			t.Errorf("parseMemoryKB(%q) = %d, want an error", bad, got)
		}
	}
}

func TestStatSpill_ConcurrentTuneAndCheckHint(t *testing.T) {
	// observeSpills runs under t.mu from Tune; CheckHint and RecordHint
	// take the same lock. Run them together (go test -race where cgo is
	// available) and check the baseline map ends consistent.
	r := newSpillReader(fourMB)
	tu := spillTuner(r, 512, 16<<30)
	ctx := context.Background()
	var wg sync.WaitGroup
	for g := int64(0); g < 8; g++ {
		wg.Add(2)
		go func(g int64) {
			defer wg.Done()
			for i := int64(1); i <= 50; i++ {
				tu.mu.Lock()
				tu.observeSpills(ctx, []candidate{spillCand(g+1, appUser, i*10, i*20_480)})
				tu.mu.Unlock()
			}
		}(g)
		go func(g int64) {
			defer wg.Done()
			for i := int64(0); i < 50; i++ {
				p := agentHint(`Set(work_mem "64MB")`)
				p.QueryID = 9_000 + g*100 + i
				if _, err := tu.CheckHint(ctx, p); err != nil {
					t.Errorf("check: %v", err)
				}
			}
		}(g)
	}
	wg.Wait()
	if n := len(tu.spill.baselines); n != 8 {
		t.Fatalf("%d baselines, want one per statement (8)", n)
	}
	for k, b := range tu.spill.baselines {
		if b.Calls != 500 || b.Blocks != 50*20_480 {
			t.Fatalf("baseline %v = %+v, want the last counters", k, b)
		}
	}
}
