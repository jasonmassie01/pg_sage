package onboarding

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
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
