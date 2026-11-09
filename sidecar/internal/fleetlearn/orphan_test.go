package fleetlearn

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// No concurrency test: PruneOrphanScopes is one transaction, serialized
// by PostgreSQL row locks; the DB tests cover it on a real server.

func TestStore_PruneOrphanScopesRejectsNonPositiveAge(t *testing.T) {
	st := NewStore(nil, "s")
	for _, age := range []time.Duration{0, -time.Hour} {
		got, err := st.PruneOrphanScopes(context.Background(), Fence{}, age)
		if err == nil || got != nil || !strings.Contains(err.Error(), "age must be positive") {
			t.Fatalf("age %v: %v err=%v, want rejected", age, got, err)
		}
	}
}

func TestStore_PruneOrphanScopesNeedsAPool(t *testing.T) {
	got, err := NewStore(nil, "s").PruneOrphanScopes(context.Background(), Fence{},
		time.Hour)
	if err == nil || got != nil || !strings.Contains(err.Error(), "prune orphan scopes") {
		t.Fatalf("nil pool = %v err=%v, want an error naming the operation", got, err)
	}
}

func TestService_OrphanPruneFailureIsLoggedNotReturned(t *testing.T) {
	var logs []string
	svc := NewService(NewStore(nil, "s"), nil, Settings{OrphanScopeAge: time.Hour},
		func(level, format string, args ...any) {
			logs = append(logs, level+" "+fmt.Sprintf(format, args...))
		})
	if err := svc.pruneOrphanScopes(context.Background(), Fence{}); err != nil {
		t.Fatalf("err = %v, want a failed prune logged, not returned", err)
	}
	if len(logs) != 1 || !strings.HasPrefix(logs[0],
		"WARN fleet learning: orphaned fleet scopes not pruned") ||
		!strings.Contains(logs[0], "prune orphan scopes") {
		t.Fatalf("logs = %q, want one WARN naming the operation", logs)
	}
}
