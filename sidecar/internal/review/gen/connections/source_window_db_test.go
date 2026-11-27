package connections

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
)

// winBase is the synthetic window's first snapshot, far from the live
// tests' now() rows.
var winBase = time.Date(2012, 3, 5, 0, 0, 0, 0, time.UTC)

// winDocs is how many snapshots fall inside the synthetic window.
const winDocs = MaxSamples + 2

// winDoc is snapshot i's workload_sessions document: api holds 4 idle
// sessions and 1 idle in a transaction for 10*i seconds; app_rw's cluster
// sessions spike to 55 at i = 100; batch has a role idle (not in a
// transaction) for a day and one aborted in a transaction for 700 s. The
// newest snapshot has no app ages (written before them), the one before an
// empty list, the one before that a truncated list.
func winDoc(i int) string {
	groups := []map[string]any{
		{"role": "app_rw", "application": "api", "state": "idle", "sessions": 4,
			"max_state_age_seconds": 30},
		{"role": "app_rw", "application": "api", "state": "idle in transaction",
			"sessions": 1, "max_state_age_seconds": 10 * i},
		{"role": "idler", "application": "batch", "state": "idle", "sessions": 1,
			"max_state_age_seconds": 86400},
		{"role": "aborter", "application": "batch", "state": "idle in transaction (aborted)",
			"sessions": 1, "max_state_age_seconds": 700}}
	peak := 5
	if i == 100 {
		peak = 55
	}
	doc := map[string]any{"groups": groups, "long": []any{},
		"roles": []map[string]any{{"role": "app_rw", "cluster_sessions": peak}}}
	switch i {
	case winDocs - 1:
	case winDocs - 2:
		doc["app_ages"] = []any{}
	default:
		doc["app_ages"] = []map[string]any{{"application": "api", "minute": i, "sessions": 5}}
		doc["app_ages_truncated"] = i == winDocs-3
	}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// storeWindow writes the window's snapshots, one a minute, plus one just
// before it and one at its end, which the read must leave out.
func storeWindow(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (from, to time.Time) {
	t.Helper()
	from, to = winBase, winBase.Add(winDocs*time.Minute)
	clear := func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM sage.snapshots
			WHERE category = 'workload_sessions' AND collected_at BETWEEN $1 AND $2`,
			from.Add(-time.Second), to); err != nil {
			t.Fatalf("clear the synthetic window: %v", err)
		}
	}
	clear()
	t.Cleanup(clear)
	put := func(at time.Time, doc string) {
		if _, err := histstore.Resolve(pool).Exec(ctx, `INSERT INTO sage.snapshots
			(collected_at, category, data{dbcol}) VALUES ($1, 'workload_sessions',
			$2::jsonb{dbval})`, at, doc); err != nil {
			t.Fatalf("store the snapshot at %s: %v", at, err)
		}
	}
	for i := 0; i < winDocs; i++ {
		put(winBase.Add(time.Duration(i)*time.Minute), winDoc(i))
	}
	put(from.Add(-time.Second), winDoc(0))
	put(to, winDoc(0))
	return from, to
}

// Integration: over a window of MaxSamples+2 snapshots the source reads
// the newest MaxSamples, oldest first, with the window's count, and its
// window-wide pass finds each role's peak and longest idle in transaction
// (aborted ones included, plain idle never) and the state mix.
func TestDBSource_WindowReadOnRealSnapshots(t *testing.T) {
	pool, ctx := livePool(t)
	from, to := storeWindow(t, ctx, pool)
	in, err := NewDBSource(histstore.Resolve(pool), nil).Read(ctx, from, to)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	checkWindowSamples(t, in)
	checkWindowRoles(t, in)
	for _, want := range []AppState{
		{Application: "api", State: "idle", Mean: 4, Peak: 4, PeakAt: winBase},
		{Application: "api", State: "idle in transaction", Mean: 1, Peak: 1, PeakAt: winBase},
	} {
		i := slices.IndexFunc(in.Mix, func(s AppState) bool {
			return s.Application == want.Application && s.State == want.State
		})
		if i < 0 || in.Mix[i].Mean != want.Mean || in.Mix[i].Peak != want.Peak ||
			!in.Mix[i].PeakAt.Equal(want.PeakAt) {
			t.Errorf("mix %+v, want %+v", in.Mix, want)
		}
	}
}

func checkWindowSamples(t *testing.T, in Inputs) {
	t.Helper()
	if in.WindowSamples != winDocs || len(in.Samples) != MaxSamples {
		t.Fatalf("read %d samples of %d, want %d of %d", len(in.Samples), in.WindowSamples,
			MaxSamples, winDocs)
	}
	for k, s := range in.Samples {
		if at := winBase.Add(time.Duration(k+2) * time.Minute); !s.At.Equal(at) {
			t.Fatalf("sample %d at %s, want %s (newest %d, oldest first)", k, s.At, at,
				MaxSamples)
		}
	}
	first, n := in.Samples[0], len(in.Samples)
	if first.Apps["api"] != 5 || len(first.Ages) != 1 || first.Ages[0].Minute == nil ||
		*first.Ages[0].Minute != 2 || first.Ages[0].Sessions != 5 || first.AgesTruncated {
		t.Errorf("oldest sample read %+v", first)
	}
	if in.Samples[n-1].Ages != nil {
		t.Errorf("newest sample (no app_ages) read ages %v, want nil", in.Samples[n-1].Ages)
	}
	if a := in.Samples[n-2].Ages; a == nil || len(a) != 0 {
		t.Errorf("empty app_ages read as %#v, want an empty non-nil list", a)
	}
	if !in.Samples[n-3].AgesTruncated || in.Samples[n-4].AgesTruncated {
		t.Errorf("truncation read as %v, %v; want true only on the third newest",
			in.Samples[n-3].AgesTruncated, in.Samples[n-4].AgesTruncated)
	}
}

func checkWindowRoles(t *testing.T, in Inputs) {
	t.Helper()
	at := winBase.Add(100 * time.Minute)
	if len(in.RolePeaks) != 1 || in.RolePeaks[0].Role != "app_rw" ||
		in.RolePeaks[0].Sessions != 55 || !in.RolePeaks[0].At.Equal(at) {
		t.Errorf("role peaks %+v, want only app_rw at 55 sessions at %s", in.RolePeaks, at)
	}
	idle := map[string]IdleInTx{}
	for _, r := range in.IdleInTx {
		idle[r.Role] = r
	}
	last := winBase.Add((winDocs - 1) * time.Minute)
	if r := idle["app_rw"]; len(idle) != 2 || r.AgeSeconds != 10*(winDocs-1) ||
		!r.At.Equal(last) {
		t.Errorf("idle in transaction %+v, want app_rw at %d s and aborter", in.IdleInTx,
			10*(winDocs-1))
	}
	if r, ok := idle["aborter"]; !ok || r.AgeSeconds != 700 || !r.At.Equal(winBase) {
		t.Errorf("aborter %+v, want 700 s first seen at %s", r, winBase)
	}
}
