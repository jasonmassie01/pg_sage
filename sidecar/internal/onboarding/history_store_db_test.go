package onboarding

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// history.store: meta. "Has pg_sage run here before" used to join the
// monitored database's sage.config with its sage.snapshots in one
// statement; with history in the store the snapshot half is read there.
func TestInstallKindIdenticalInBothPlacements(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		reset(t, p)
		p.Noise(t, time.Now(), 1) // another database's history: still a new install
		st, err := Init(ctx, p.Monitored, "fresh")
		if err != nil {
			t.Fatalf("%s init fresh: %v", mode, err)
		}
		if st.InstallKind != InstallNew {
			t.Fatalf("%s: no snapshot of its own: want new, got %s", mode, st.InstallKind)
		}
		if _, err := histstore.Resolve(p.Monitored).Exec(ctx, `INSERT INTO sage.snapshots
			(collected_at, category, data{dbcol}) VALUES (now(), 'system', '{}'{dbval})`); err != nil {
			t.Fatal(err)
		}
		st, err = Init(ctx, p.Monitored, "upgraded")
		if err != nil {
			t.Fatalf("%s init upgraded: %v", mode, err)
		}
		if st.InstallKind != InstallExisting {
			t.Fatalf("%s: a snapshot of its own marks an existing install, got %s", mode,
				st.InstallKind)
		}
	}
}

func TestInstallKindTrustRampAloneMarksExisting(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMeta)
	reset(t, p)
	ctx := context.Background()
	if _, err := p.Monitored.Exec(ctx, `INSERT INTO sage.config (key, value, updated_by)
		VALUES ('trust_ramp_start', now()::text, 'test')`); err != nil {
		t.Fatal(err)
	}
	st, err := Init(ctx, p.Monitored, "ramped")
	if err != nil {
		t.Fatal(err)
	}
	if st.InstallKind != InstallExisting {
		t.Fatalf("a trust ramp on the monitored database marks existing, got %s",
			st.InstallKind)
	}
}

func reset(t *testing.T, p *histfixture.Pair) {
	t.Helper()
	if _, err := p.Monitored.Exec(context.Background(), `DELETE FROM sage.onboarding;
		DELETE FROM sage.config WHERE key = 'trust_ramp_start'`); err != nil {
		t.Fatal(err)
	}
}
