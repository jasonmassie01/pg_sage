package main

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/histstore"
)

// No database, concurrency or invalid-input tests: the config value was
// validated when it was loaded, and startup sets the mode once.

// setHistoryProcessMode follows history.store: meta makes the process meta
// (unregistered pools refused), monitored and the empty default keep it
// monitored.
func TestSetHistoryProcessModeFollowsConfig(t *testing.T) {
	preserveHistoryGlobals(t)
	t.Cleanup(func() {
		if err := histstore.SetProcessMode(histstore.ModeMonitored); err != nil {
			t.Error(err)
		}
	})
	for _, tc := range []struct {
		store string
		want  histstore.Mode
	}{{"meta", histstore.ModeMeta}, {"monitored", histstore.ModeMonitored},
		{"meta", histstore.ModeMeta}, {"", histstore.ModeMonitored}} {
		cfg.History.Store = tc.store
		setHistoryProcessMode()
		if got := histstore.ProcessMode(); got != tc.want {
			t.Fatalf("history.store %q: process mode %q, want %q", tc.store, got, tc.want)
		}
	}
}
