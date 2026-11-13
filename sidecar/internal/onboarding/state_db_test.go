package onboarding

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestInitNewInstall(t *testing.T) {
	pool, ctx := freshInstall(t)
	st, err := Init(ctx, pool, "app")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if st.Database != "app" || st.InstallKind != InstallNew || st.InstalledAt.IsZero() ||
		st.FirstLookAt != nil || st.FirstFindingAt != nil || st.TTFFSeconds != nil {
		t.Fatalf("state = %+v, want a new install with nothing recorded", st)
	}
	// A restart of the same install keeps its kind: the trust ramp row the
	// executor writes later must not turn it into an "existing" install.
	if _, err := pool.Exec(ctx, `INSERT INTO sage.config (key, value, updated_by)
		VALUES ('trust_ramp_start', now()::text, 'bootstrap')`); err != nil {
		t.Fatalf("ramp row: %v", err)
	}
	again, err := Init(ctx, pool, "app")
	if err != nil || again.InstallKind != InstallNew || !again.InstalledAt.Equal(st.InstalledAt) {
		t.Fatalf("restart state = %+v err %v, want the original new install", again, err)
	}
}

func TestInitExistingInstall(t *testing.T) {
	for name, marker := range map[string]string{
		"trust ramp started": `INSERT INTO sage.config (key, value, updated_by)
			VALUES ('trust_ramp_start', '2026-01-01T00:00:00Z', 'bootstrap')`,
		"snapshots collected": `INSERT INTO sage.snapshots (category, data)
			VALUES ('system', '{}'::jsonb)`,
	} {
		t.Run(name, func(t *testing.T) {
			pool, ctx := freshInstall(t)
			if _, err := pool.Exec(ctx, marker); err != nil {
				t.Fatalf("marker: %v", err)
			}
			st, err := Init(ctx, pool, "app")
			if err != nil || st.InstallKind != InstallExisting {
				t.Fatalf("state = %+v err %v, want existing", st, err)
			}
		})
	}
}

func TestInitIsRaceFree(t *testing.T) {
	pool, ctx := freshInstall(t)
	var wg sync.WaitGroup
	kinds := make([]InstallKind, 8)
	errs := make([]error, 8)
	for i := range kinds {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, err := Init(ctx, pool, "app")
			kinds[i], errs[i] = st.InstallKind, err
		}(i)
	}
	wg.Wait()
	for i := range kinds {
		if errs[i] != nil || kinds[i] != InstallNew {
			t.Fatalf("init %d: kind %q err %v", i, kinds[i], errs[i])
		}
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM sage.onboarding").Scan(&n); err != nil ||
		n != 1 {
		t.Fatalf("onboarding rows = %d err %v, want 1", n, err)
	}
}

func TestRecordFirstLookAndFinding(t *testing.T) {
	pool, ctx := freshInstall(t)
	if _, err := Init(ctx, pool, "app"); err != nil {
		t.Fatalf("init: %v", err)
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	if err := RecordFirstLook(ctx, pool, "app", at); err != nil {
		t.Fatalf("first look: %v", err)
	}
	ok, err := RecordFirstFinding(ctx, pool, "app", at, 12500*time.Millisecond, "first_look")
	if err != nil || !ok {
		t.Fatalf("first finding: ok=%v err=%v", ok, err)
	}
	// Only the first one counts.
	ok, err = RecordFirstFinding(ctx, pool, "app", at.Add(time.Hour), time.Hour, "analyzer")
	if err != nil || ok {
		t.Fatalf("second finding: ok=%v err=%v, want not recorded", ok, err)
	}
	if err := RecordFirstLook(ctx, pool, "app", at.Add(time.Hour)); err != nil {
		t.Fatalf("second first look: %v", err)
	}
	st, found, err := Get(ctx, pool, "app")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if st.FirstLookAt == nil || !st.FirstLookAt.Equal(at) || st.FirstFindingAt == nil ||
		!st.FirstFindingAt.Equal(at) || st.FirstFindingSource != "first_look" ||
		st.TTFFSeconds == nil || *st.TTFFSeconds != 12.5 {
		t.Fatalf("state = %+v, want the first look and finding at %s, ttff 12.5 s", st, at)
	}
}

func TestGetUnknownAndErrors(t *testing.T) {
	pool, ctx := freshInstall(t)
	if _, found, err := Get(ctx, pool, "nobody"); err != nil || found {
		t.Fatalf("unknown database: found=%v err=%v", found, err)
	}
	if _, err := Init(ctx, nil, "app"); !errors.Is(err, ErrNoPool) {
		t.Fatalf("nil pool err = %v", err)
	}
	if _, err := Init(ctx, pool, ""); !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("empty database err = %v", err)
	}
	if _, err := RecordFirstFinding(ctx, pool, "nobody", time.Now(), time.Second,
		"first_look"); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("finding before init err = %v, want ErrNotInitialized", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Init(cancelled, pool, "app"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled init err = %v", err)
	}
}

func TestHasOpenFinding(t *testing.T) {
	pool, ctx := freshInstall(t)
	if open, err := HasOpenFinding(ctx, pool); err != nil || open {
		t.Fatalf("empty findings: open=%v err=%v", open, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.findings (category, severity, object_type,
		object_identifier, title, detail, status) VALUES ('duplicate_index', 'warning',
		'index', 'public.x', 'dup', '{}'::jsonb, 'open')`); err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	if open, err := HasOpenFinding(ctx, pool); err != nil || !open {
		t.Fatalf("one open finding: open=%v err=%v", open, err)
	}
}

// seededFindings numbers seeded findings: open ones are unique per object.
var seededFindings atomic.Int64

// seedFinding inserts one sage.findings row of severity and status.
func seedFinding(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	severity, status string) {
	t.Helper()
	object := fmt.Sprintf("public.x%d", seededFindings.Add(1))
	if _, err := pool.Exec(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, status)
		VALUES ('duplicate_index', $1, 'index', $3, 'dup', '{}'::jsonb, $2)`,
		severity, status, object); err != nil {
		t.Fatalf("seed %s %s finding: %v", status, severity, err)
	}
}

// TestHasOpenFindingCountsOnlyWarningAndCritical pins firstlook-07b: the
// analyzer's first finding is an open warning or critical, as the first
// look counts them; an info note, a legacy upper-case severity or a closed
// finding is not one.
func TestHasOpenFindingCountsOnlyWarningAndCritical(t *testing.T) {
	type seed struct{ severity, status string }
	cases := []struct {
		name  string
		seeds []seed
		want  bool
	}{
		{"no findings", nil, false},
		{"open warning", []seed{{"warning", "open"}}, true},
		{"open critical", []seed{{"critical", "open"}}, true},
		{"open info only", []seed{{"info", "open"}, {"info", "open"}}, false},
		{"legacy upper-case INFO", []seed{{"INFO", "open"}}, false},
		{"upper-case WARNING is not exact", []seed{{"WARNING", "open"}}, false},
		{"info and a resolved critical",
			[]seed{{"info", "open"}, {"critical", "resolved"}}, false},
		{"suppressed warning", []seed{{"warning", "suppressed"}}, false},
		{"info and an open critical", []seed{{"info", "open"}, {"critical", "open"}}, true},
	}
	pool, ctx := freshInstall(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, "DELETE FROM sage.findings"); err != nil {
				t.Fatalf("clear findings: %v", err)
			}
			for _, s := range tc.seeds {
				seedFinding(t, ctx, pool, s.severity, s.status)
			}
			open, err := HasOpenFinding(ctx, pool)
			if err != nil || open != tc.want {
				t.Fatalf("HasOpenFinding = %v err %v, want %v", open, err, tc.want)
			}
		})
	}
}

func TestHasOpenFindingErrors(t *testing.T) {
	if open, err := HasOpenFinding(context.Background(), nil); !errors.Is(err, ErrNoPool) ||
		open {
		t.Fatalf("nil pool: open=%v err=%v, want ErrNoPool", open, err)
	}
	pool, ctx := freshInstall(t)
	seedFinding(t, ctx, pool, "critical", "open")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if open, err := HasOpenFinding(cancelled, pool); !errors.Is(err, context.Canceled) ||
		open {
		t.Fatalf("cancelled: open=%v err=%v, want context.Canceled", open, err)
	}
	if _, err := pool.Exec(ctx, "ALTER TABLE sage.findings RENAME TO findings_gone"); err != nil {
		t.Fatalf("hide findings: %v", err)
	}
	open, err := HasOpenFinding(ctx, pool)
	var pgErr *pgconn.PgError
	if open || err == nil || !strings.Contains(err.Error(), "read open findings") ||
		!errors.As(err, &pgErr) || pgErr.Code != "42P01" {
		t.Fatalf("missing table: open=%v err=%v, want a wrapped undefined_table", open, err)
	}
}
