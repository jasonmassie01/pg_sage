package srebench

import (
	"testing"
	"time"
)

// The archiver program on a server without the archive fixture is
// skipped, but the harness still recovers it. Recovery must then return
// at once instead of waiting 90 s for an archiver that is off (it cost
// about 3 minutes of every bench run on such servers, CI included).
func TestArchiverFailure_RecoverWithoutTheFixtureIsImmediate(t *testing.T) {
	ctx, env := liveEnv(t)
	var mode string
	if err := env.Pool.QueryRow(ctx, "SHOW archive_mode").Scan(&mode); err != nil {
		t.Fatalf("archive_mode: %v", err)
	}
	if mode == "on" {
		t.Skip("this server has archiving on; the fixture may be configured")
	}
	start := time.Now()
	if err := archiverFailure().Recover(ctx, env); err != nil {
		t.Fatalf("recover without the fixture: %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("recover without the fixture took %s", d)
	}
}
