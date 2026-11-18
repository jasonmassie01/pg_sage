package config

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Happy path: temp files at 2GiB an hour with work_mem at its default give
// one candidate: work_mem doubled (8MB, under the memory cap of 16GB / 4 /
// 100 connections), citing the setting (whose fact names its measure),
// max_connections and the memory it is sized against.
func TestGenerate_WorkMemAgainstTempSpills(t *testing.T) {
	f := newFixture()
	f.measures.TempBytesPerHour = 2 * gib
	cands, cov := generate(t, f)
	c := only(t, cands, KindWorkMem)
	if c.ActionType != "alter_system_guc" || c.ForwardSQL != "ALTER SYSTEM SET work_mem = '8MB'" ||
		c.RollbackSQL != "ALTER SYSTEM RESET work_mem" || !c.Reversible {
		t.Fatalf("work_mem step %+v", c)
	}
	if !hasEvidence(c, "setting/work_mem", "setting/max_connections", "provider/memory_bytes") {
		t.Fatalf("evidence %v", c.EvidenceIDs)
	}
	q := c.Prediction.Quantities
	if q["temp_bytes_per_hour"] != 2*gib || q["current_bytes"] != 4<<20 ||
		q["proposed_bytes"] != 8<<20 {
		t.Fatalf("quantities %v", q)
	}
	if c.Approval != candidate.ApprovalRequired {
		t.Fatalf("a global setting step is %q, want approval_required", c.Approval)
	}
	if cov.Status != candidate.CoverageComplete {
		t.Fatalf("coverage %+v", cov)
	}
	if Contradicted(cands) != 1 {
		t.Fatalf("contradicted %d, want 1", Contradicted(cands))
	}
}

// Boundary: the spill floor is 256MiB an hour, inclusive.
func TestGenerate_WorkMemSpillFloor(t *testing.T) {
	f := newFixture()
	f.measures.TempBytesPerHour = float64(256 << 20)
	cands, _ := generate(t, f)
	only(t, cands, KindWorkMem)
	f.measures.TempBytesPerHour = float64(256<<20) - 1
	cands, _ = generate(t, f)
	none(t, cands, KindWorkMem)
}

// A work_mem already at its memory cap (1GB / 4 / 100 = 2.6MB, under the
// current 4MB) gives no candidate and says why.
func TestGenerate_WorkMemAtItsMemoryCap(t *testing.T) {
	f := newFixture()
	f.memoryBytes = 1 * gib
	f.measures.TempBytesPerHour = 2 * gib
	cands, cov := generate(t, f)
	none(t, cands, KindWorkMem)
	if !notesContain(cov, "work_mem", "memory cap") {
		t.Fatalf("notes %v", cov.Notes)
	}
}

// shared_buffers is judged against memory: under 15% or over 40% is a
// contradiction, exactly 15% and exactly 40% are not. It needs a restart,
// so on a self-managed server it is an operator step that pg_sage never
// runs, landing when SHOW prints the new value.
func TestGenerate_SharedBuffersAgainstMemory(t *testing.T) {
	cases := []struct {
		pages, want string // shared_buffers in 8kB pages; "" = no candidate
	}{
		{"393216", ""},       // 3GB = 15% of 20GB
		{"393215", "5GB"},    // just under 15%
		{"1048576", ""},      // 8GB = 40%
		{"1048577", "5GB"},   // just over 40%
		{"16384", "5GB"},     // 128MB
		{"not-a-number", ""}, // unparseable: skipped
	}
	for _, tc := range cases {
		f := newFixture().with("shared_buffers", tc.pages, "8kB", "configuration file")
		f.memoryBytes = 20 * gib
		cands, _ := generate(t, f)
		if tc.want == "" {
			none(t, cands, KindSharedBuffers)
			continue
		}
		c := only(t, cands, KindSharedBuffers)
		op := c.Operator
		if c.ActionType != candidate.ActionOperator || op == nil || op.Value != tc.want ||
			op.Setting != "shared_buffers" || !op.RebootRequired ||
			!strings.Contains(op.Command, "ALTER SYSTEM SET shared_buffers = '"+tc.want+"'") ||
			!strings.Contains(op.Command, "restart") || c.ForwardSQL != "" || c.Reversible {
			t.Fatalf("pages %s: %+v (operator %+v)", tc.pages, c, op)
		}
	}
}

// Invalid input: a computed value above the pgconf maximum is clamped to
// it (25% of 4TB is 1TB; shared_buffers' maximum is 256GB).
func TestGenerate_ClampsAboveTheMaximum(t *testing.T) {
	f := newFixture().with("shared_buffers", "16384", "8kB", "configuration file")
	f.memoryBytes = 4096 * gib
	cands, _ := generate(t, f)
	c := only(t, cands, KindSharedBuffers)
	if c.Operator.Value != "256GB" {
		t.Fatalf("value %q, want the clamped 256GB", c.Operator.Value)
	}
}

// max_wal_size against checkpoints: more requested than timed checkpoints
// sizes max_wal_size to hold one checkpoint_timeout of WAL times (1 +
// checkpoint_completion_target): 20GiB/h * 300s/3600s * 1.9 = 3.17GB,
// rounded up to 4GB. As many requested as timed is no contradiction.
func TestGenerate_MaxWALSizeAgainstCheckpoints(t *testing.T) {
	f := newFixture()
	f.measures.CheckpointsRequested, f.measures.CheckpointsTimed = 30.0, 10.0
	f.measures.WALBytesPerHour = 20 * gib
	cands, _ := generate(t, f)
	c := only(t, cands, KindMaxWALSize)
	if c.ForwardSQL != "ALTER SYSTEM SET max_wal_size = '4GB'" ||
		!hasEvidence(c, "setting/max_wal_size", "setting/checkpoint_timeout") {
		t.Fatalf("max_wal_size step %+v", c)
	}
	f.measures.CheckpointsRequested = 10.0
	cands, _ = generate(t, f)
	none(t, cands, KindMaxWALSize)
}

// random_page_cost against storage: a managed service's storage is SSD,
// so 4 becomes 1.1 through the provider's parameter group (an operator
// step); on a self-managed server the storage class is not known, so
// nothing is proposed and the coverage says why.
func TestGenerate_RandomPageCostAgainstStorage(t *testing.T) {
	f := newFixture()
	f.platform = "rds"
	cands, _ := generate(t, f)
	c := only(t, cands, KindRandomPageCost)
	if c.ActionType != candidate.ActionOperator || c.Operator.Value != "1.1" ||
		c.Operator.Provider != "rds" ||
		!strings.Contains(c.Operator.Command, "modify-db-parameter-group") {
		t.Fatalf("random_page_cost on rds %+v (operator %+v)", c, c.Operator)
	}
	cands, cov := generate(t, newFixture())
	none(t, cands, KindRandomPageCost)
	if !notesContain(cov, "random_page_cost", "storage") {
		t.Fatalf("notes %v", cov.Notes)
	}
}

// PostgreSQL 18 I/O: an effective_io_concurrency of 1 carried over from an
// older server becomes 18's default 16; io_method = sync is advice only.
// On 17 neither rule applies.
func TestGenerate_PG18IO(t *testing.T) {
	f := newFixture().with("effective_io_concurrency", "1", "", "configuration file").
		with("io_method", "sync", "", "configuration file")
	f.version = "18.1"
	cands, _ := generate(t, f)
	c := only(t, cands, KindIOConcurrency)
	if c.ForwardSQL != "ALTER SYSTEM SET effective_io_concurrency = '16'" ||
		c.RollbackSQL != "ALTER SYSTEM SET effective_io_concurrency = '1'" {
		t.Fatalf("effective_io_concurrency step %+v", c)
	}
	m := only(t, cands, KindIOMethod)
	if m.ActionType != candidate.ActionAdvice || m.ForwardSQL != "" || m.Operator != nil ||
		!strings.Contains(m.Reason, "worker") {
		t.Fatalf("io_method advice %+v", m)
	}
	f.version = "17.6"
	cands, _ = generate(t, f)
	none(t, cands, KindIOConcurrency)
	none(t, cands, KindIOMethod)
}

// idle_in_transaction_session_timeout off while sessions sit idle in a
// transaction is advice (it is set per database, with approval, and the
// pack does not name the server's database), citing the busiest hour.
func TestGenerate_IdleInTransaction(t *testing.T) {
	f := newFixture()
	hours := make([]workpack.ConnectionHour, 168)
	for i := range hours {
		hours[i] = workpack.ConnectionHour{ID: connHourID(i), Hour: i, States: "unknown",
			Ages: "unknown"}
	}
	hours[10].Snapshots, hours[10].States = 6, map[string]any{"idle in transaction": 3.5,
		"active": 2.0}
	hours[11].Snapshots, hours[11].States = 6, map[string]any{"idle in transaction": 1.0}
	f.connections = &workpack.Connections{Hours: hours}
	cands, _ := generate(t, f)
	c := only(t, cands, KindIdleInTransaction)
	if c.ActionType != candidate.ActionAdvice || !hasEvidence(c,
		"setting/idle_in_transaction_session_timeout", "connections/hour/10") ||
		!strings.Contains(c.Reason, "30min") {
		t.Fatalf("idle advice %+v", c)
	}
	f = f.with("idle_in_transaction_session_timeout", "600000", "ms", "database")
	cands, _ = generate(t, f)
	none(t, cands, KindIdleInTransaction)
}

// Every global-setting candidate carries lab-01's marker: no control
// group, an interrupted time series against the seasonal baseline.
func TestGenerate_GlobalSettingsCarryTheNoControlMarker(t *testing.T) {
	f := newFixture().with("shared_buffers", "16384", "8kB", "configuration file")
	f.measures.TempBytesPerHour = 2 * gib
	cands, _ := generate(t, f)
	if len(cands) < 2 {
		t.Fatalf("want work_mem and shared_buffers candidates, got %+v", cands)
	}
	for _, c := range cands {
		if !strings.Contains(c.Prediction.Source, verify.ReasonGlobal) ||
			!strings.Contains(c.Prediction.Source, verify.MethodITS) {
			t.Fatalf("%s prediction source %q lacks the no-control marker", c.Kind,
				c.Prediction.Source)
		}
		if c.Prediction.Tier != candidate.TierNone || c.Dimension != Dimension {
			t.Fatalf("%s prediction %+v dimension %s", c.Kind, c.Prediction, c.Dimension)
		}
	}
}

// Every non-default setting is listed as a fact, by name; a name that is
// not a setting name is skipped with a reason.
func TestNonDefaults_ListsEveryNonDefaultSetting(t *testing.T) {
	f := newFixture().with("Bad Name!", "1", "", "configuration file")
	got, err := NonDefaults(f.pack(t))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range got {
		names = append(names, s.Name+"="+s.ID)
	}
	want := "max_connections=setting/max_connections|shared_buffers=setting/shared_buffers"
	if strings.Join(names, "|") != want {
		t.Fatalf("non-defaults %v, want %s", names, want)
	}
	_, cov := generate(t, f)
	if !notesContain(cov, `"Bad Name!"`, "skipped") {
		t.Fatalf("notes %v", cov.Notes)
	}
}

// Nil and empty: a nil pack is an error, a pack without the settings
// section gives no candidates and a coverage gap, a rule whose setting is
// absent from the section gives no candidate, and an invalid env is
// refused.
func TestGenerate_NilEmptyAndMissing(t *testing.T) {
	if _, _, err := New().Generate(context.Background(), nil, env); err == nil {
		t.Fatal("a nil pack was not an error")
	}
	f := newFixture()
	f.settings = nil
	cands, cov := generate(t, f)
	if len(cands) != 0 || cov.Status != candidate.CoveragePartial ||
		strings.Join(cov.Missing, ",") != "settings" {
		t.Fatalf("no settings section: %d candidates, coverage %+v", len(cands), cov)
	}
	if _, err := NonDefaults(f.pack(t)); err != nil {
		t.Fatalf("NonDefaults without a settings section: %v", err)
	}
	g := newFixture()
	g.measures.TempBytesPerHour = 2 * gib
	g.settings = g.settings[:len(g.settings)-1] // drop work_mem
	cands, _ = generate(t, g)
	none(t, cands, KindWorkMem)
	if _, _, err := New().Generate(context.Background(), newFixture().pack(t),
		candidate.Env{}); err == nil {
		t.Fatal("an env naming no database was accepted")
	}
	h := newFixture()
	h.memoryBytes = nil
	h.measures.TempBytesPerHour = 2 * gib
	cands, cov = generate(t, h)
	only(t, cands, KindWorkMem) // doubled, uncapped by memory
	if !strings.Contains(strings.Join(cov.Missing, ","), "memory") {
		t.Fatalf("no memory fact: coverage %+v", cov)
	}
}

// Error propagation: on a managed service managedparam cannot describe
// (Neon, Supabase) or an unknown platform, managedparam.NewIntent fails,
// so the candidate is advice carrying that reason, never alter_system_guc.
func TestGenerate_UnsupportedProviderIsAdvice(t *testing.T) {
	for _, platform := range []string{"neon", "supabase", ""} {
		f := newFixture()
		f.platform = platform
		f.measures.TempBytesPerHour = 2 * gib
		cands, _ := generate(t, f)
		c := only(t, cands, KindWorkMem)
		if c.ActionType != candidate.ActionAdvice || c.ForwardSQL != "" ||
			!strings.Contains(c.Reason, "not supported") ||
			!strings.Contains(c.Reason, "8MB") {
			t.Fatalf("platform %q: %+v", platform, c)
		}
	}
}

// A managed parameter group service gets the provider's own command for a
// reloadable setting too: ALTER SYSTEM is not available there.
func TestGenerate_ManagedWorkMemIsAnOperatorStep(t *testing.T) {
	f := newFixture()
	f.platform = "rds"
	f.measures.TempBytesPerHour = 2 * gib
	cands, _ := generate(t, f)
	c := only(t, cands, KindWorkMem)
	op := c.Operator
	if c.ActionType != candidate.ActionOperator || op == nil || op.Value != "8MB" ||
		op.Setting != "work_mem" || op.Provider != "rds" || op.RebootRequired ||
		!strings.Contains(op.Command, "work_mem") || op.Revert == "" {
		t.Fatalf("managed work_mem %+v (operator %+v)", c, op)
	}
}

// Concurrency: the generator is a pure function of the pack, so the same
// pack generated in parallel gives the same candidate ids.
func TestGenerate_ParallelRunsAgree(t *testing.T) {
	f := newFixture().with("shared_buffers", "16384", "8kB", "configuration file")
	f.measures.TempBytesPerHour = 2 * gib
	p := f.pack(t)
	ids := make([]string, 8)
	done := make(chan struct{})
	for i := range ids {
		go func() {
			defer func() { done <- struct{}{} }()
			cands, _, err := New().Generate(context.Background(), p, env)
			if err != nil {
				t.Errorf("run %d: %v", i, err)
				return
			}
			for _, c := range cands {
				ids[i] += c.ID
			}
		}()
	}
	for range ids {
		<-done
	}
	for i := range ids {
		if ids[i] == "" || ids[i] != ids[0] {
			t.Fatalf("run %d ids %q differ from run 0 %q", i, ids[i], ids[0])
		}
	}
}

func notesContain(cov candidate.Coverage, parts ...string) bool {
	for _, n := range cov.Notes {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(n, p)
		}
		if ok {
			return true
		}
	}
	return false
}

func connHourID(i int) string {
	return "connections/hour/" + strconv.Itoa(i)
}
