package advisor

import (
	"testing"
)

func TestRequiresRestart_MaxConnections(t *testing.T) {
	if !RequiresRestart("max_connections") {
		t.Fatal("expected max_connections to require restart")
	}
}

func TestRequiresRestart_SharedBuffers(t *testing.T) {
	if !RequiresRestart("shared_buffers") {
		t.Fatal("expected shared_buffers to require restart")
	}
}

func TestRequiresRestart_WorkMem(t *testing.T) {
	if RequiresRestart("work_mem") {
		t.Fatal("expected work_mem to not require restart")
	}
}

func TestRequiresRestart_WalBuffers(t *testing.T) {
	if !RequiresRestart("wal_buffers") {
		t.Fatal("expected wal_buffers to require restart")
	}
}

func TestRequiresRestart_Unknown(t *testing.T) {
	if RequiresRestart("nonexistent_setting") {
		t.Fatal("expected unknown setting to not require restart")
	}
}

// G-P0-1: autovacuum_max_workers is postmaster-context before PG18; the
// old advisor list missed it, so a restart-only change looked reloadable.
func TestRequiresRestart_AutovacuumMaxWorkers(t *testing.T) {
	if !RequiresRestart("autovacuum_max_workers") {
		t.Error("autovacuum_max_workers should require restart")
	}
}
