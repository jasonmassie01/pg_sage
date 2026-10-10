package main

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/decommission"
)

// Without a control pool the startup inventory cannot run: it says so as an
// error naming §12 and the API route, and the sidecar keeps starting.
func TestRunDecommissionReport_NoPoolLogsAnErrorAndReturns(t *testing.T) {
	out := captureStderr(t, func() {
		runDecommissionReport(context.Background(), nil, "")
	})
	for _, want := range []string{"[ERROR] [decommission]", "§12",
		decommission.InventoryPath, "no control database pool"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log %q lacks %q", out, want)
		}
	}
}

func TestRunDecommissionReport_CanceledContextIsAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := captureStderr(t, func() { runDecommissionReport(ctx, nil, "") })
	if strings.Count(out, "[ERROR]") != 1 {
		t.Fatalf("want exactly one error line, got %q", out)
	}
}
