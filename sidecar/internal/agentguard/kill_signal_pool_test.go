package agentguard

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// No concurrent access tests: signalPool reads two fields of a value the
// kill never mutates.

func killTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(),
		"postgres://u@127.0.0.1:1/db?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// Nil/empty: without an action identity the kill signals on pg_sage's
// pool (single identity, today's behaviour).
func TestKillTargetSignalPool_SingleIdentity(t *testing.T) {
	obs := killTestPool(t)
	if got := (KillTarget{Pool: obs}).signalPool(); got != obs {
		t.Fatalf("signalPool = %p, want the observation pool %p", got, obs)
	}
	if got := (KillTarget{}).signalPool(); got != nil {
		t.Fatalf("signalPool of an empty target = %p, want nil", got)
	}
}

// Happy path: with an action identity, cancel and terminate take the
// action pool; the observation pool keeps the reads and sage writes.
func TestKillTargetSignalPool_ActionIdentity(t *testing.T) {
	obs, act := killTestPool(t), killTestPool(t)
	target := KillTarget{Pool: obs, SignalPool: act}
	if got := target.signalPool(); got != act {
		t.Fatalf("signalPool = %p, want the action pool %p", got, act)
	}
	if target.Pool != obs {
		t.Fatal("the observation pool was replaced")
	}
}
