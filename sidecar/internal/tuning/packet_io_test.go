package tuning

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// market-15: the tuning agent's packet lists PostgreSQL 18's I/O settings
// beside effective_io_concurrency, so it reasons with them.

// No concurrent access tests: writeSettings fills one builder per call.

func TestWriteSettings_ListsTheIOSettings(t *testing.T) {
	snap := &collector.Snapshot{ConfigData: &collector.ConfigSnapshot{
		PGSettings: []collector.PGSetting{
			{Name: "io_method", Setting: "sync", Source: "configuration file",
				Context: "postmaster"},
			{Name: "io_workers", Setting: "3", Source: "default", Context: "sighup"},
			{Name: "maintenance_io_concurrency", Setting: "10",
				Source: "configuration file", Context: "user"},
			{Name: "effective_io_concurrency", Setting: "1",
				Source: "configuration file", Context: "user"},
			{Name: "io_combine_limit", Setting: "16", Unit: "8kB", Source: "default",
				Context: "user"},
		}}}
	p := &packetBuilder{evidence: evidenceSet{}, limit: 1 << 16}
	writeSettings(p, snap)
	text := p.text()
	for _, want := range []string{
		"io_method = sync (source configuration file, context postmaster)",
		"io_workers = 3 (source default, context sighup)",
		"maintenance_io_concurrency = 10 (source configuration file, context user)",
		"effective_io_concurrency = 1 (source configuration file, context user)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("packet settings lack %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "io_combine_limit") {
		t.Errorf("io_combine_limit is not a packet setting:\n%s", text)
	}
}

// A server before 18 has no io_method row: the packet lists what exists.
func TestWriteSettings_AbsentIOSettingsAreSkipped(t *testing.T) {
	snap := &collector.Snapshot{ConfigData: &collector.ConfigSnapshot{
		PGSettings: []collector.PGSetting{{Name: "maintenance_io_concurrency",
			Setting: "10", Source: "default", Context: "user"}}}}
	p := &packetBuilder{evidence: evidenceSet{}, limit: 1 << 16}
	writeSettings(p, snap)
	text := p.text()
	if !strings.Contains(text, "maintenance_io_concurrency = 10") ||
		strings.Contains(text, "io_method") {
		t.Errorf("packet = %q, want only the setting the server has", text)
	}
}
