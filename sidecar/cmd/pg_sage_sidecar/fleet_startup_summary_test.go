package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// No concurrent calls: bootstrap and stderr are process-global startup state.
// Real successful database initialization is checked by the fleet process scenarios.
func TestFleetStartupSummaryDoesNotCountFailedDatabases(t *testing.T) {
	for _, configured := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("configured_%d", configured), func(t *testing.T) {
			preserveFleetRuntimeGlobals(t)
			cfg = config.DefaultConfig()
			cfg.Mode = "fleet"
			configController = nil
			cfg.Databases = nil
			for i := 0; i < configured; i++ {
				cfg.Databases = append(cfg.Databases, config.DatabaseConfig{
					Name: fmt.Sprintf("invalid-%d", i), Host: "127.0.0.1",
					Database: "unavailable", SSLMode: "invalid-mode",
				})
			}
			output := captureFleetStartupLog(t)
			want := fmt.Sprintf("0 of %d configured databases initialized", configured)
			if !strings.Contains(output, want) {
				t.Fatalf("startup must distinguish failures; want %q, got %s", want, output)
			}
			if got := fleetMgr.InstanceCount(); got != configured {
				t.Fatalf("failed definitions must remain visible: got %d, want %d", got, configured)
			}
			for _, instance := range fleetMgr.Instances() {
				if instance.Pool != nil || instance.Status.Error == "" {
					t.Fatal("invalid configuration lost its disconnected error state")
				}
			}
		})
	}
}

func captureFleetStartupLog(t *testing.T) string {
	t.Helper()
	previous := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	os.Stderr = writer
	defer func() { os.Stderr = previous }()
	initFleetMultiDB()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}
