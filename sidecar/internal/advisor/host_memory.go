package advisor

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

// HostMemory is the host's RAM as managed-cloud telemetry (or the
// operator) reports it; zero means unknown.
type HostMemory struct {
	TotalBytes     int64
	AvailableBytes int64
}

// HostMemorySource reads current host memory (managed-cloud telemetry).
type HostMemorySource func() HostMemory

const (
	// maxWorkMemPct caps work_mem against host RAM: one sort or hash per
	// backend node is allowed this share of memory.
	maxWorkMemPct = 5
	// minAvailablePct is memory pressure: below it no memory setting may
	// grow.
	minAvailablePct = 5
)

// WithHostMemorySource supplies live host memory; an unknown reading
// falls back to WithHostMemoryBytes.
func (a *Advisor) WithHostMemorySource(src HostMemorySource) {
	a.hostMemorySource = src
}

func (a *Advisor) hostMemory() HostMemory {
	if a.hostMemorySource != nil {
		if m := a.hostMemorySource(); m.TotalBytes > 0 {
			return m
		}
	}
	return HostMemory{TotalBytes: a.hostMemoryBytes}
}

// GateConfigFindingsHost is GateConfigFindings with live host memory: the
// shared_buffers grounding, the work_mem cap and the memory-pressure
// guard. Unknown memory keeps the behavior of GateConfigFindings.
func GateConfigFindingsHost(findings []analyzer.Finding, host HostMemory,
	cloudEnv, dbName string, settings []collector.PGSetting) []analyzer.Finding {
	kept := make([]analyzer.Finding, 0, len(findings))
	for _, f := range findings {
		if ok, _ := ValidateConfigSQL(f.RecommendedSQL); ok {
			kept = append(kept, f)
		}
	}
	kept = applyHostMemoryGuard(kept, host.TotalBytes)
	kept = applyWorkMemGuard(kept, host)
	kept = applyMemoryPressureGuard(kept, host, settings)
	kept = applyConfigAllowlist(kept)
	return TransformForCloud(kept, cloudEnv, dbName, settings)
}

// memorySetting parses an executable ALTER SYSTEM SET of a memory GUC.
func memorySetting(f analyzer.Finding) (name string, bytes float64, ok bool) {
	stmt, isSet := pgconf.ParseAlterSystem(f.RecommendedSQL)
	if !isSet || stmt.Reset {
		return "", 0, false
	}
	doc, documented := pgconf.Docs[stmt.Name]
	if !documented || doc.Unit != "bytes" || stmt.Name == "effective_cache_size" {
		return "", 0, false
	}
	v, err := pgconf.ParseValue(stmt.Value, doc)
	if err != nil {
		return "", 0, false
	}
	return stmt.Name, v, true
}

func notExecutable(f analyzer.Finding, reason string) analyzer.Finding {
	f.RecommendedSQL, f.RollbackSQL, f.ActionRisk = "", "", ""
	f.Severity = "info"
	f.Recommendation += " (Not executable: " + reason + ".)"
	return f
}

// applyWorkMemGuard refuses work_mem above maxWorkMemPct of host RAM.
func applyWorkMemGuard(findings []analyzer.Finding, host HostMemory) []analyzer.Finding {
	if host.TotalBytes <= 0 {
		return findings
	}
	for i, f := range findings {
		name, v, ok := memorySetting(f)
		if !ok || name != "work_mem" || v*100 <= float64(host.TotalBytes)*maxWorkMemPct {
			continue
		}
		findings[i] = notExecutable(f, fmt.Sprintf("work_mem above %d%% of host memory "+
			"(%d MiB) can exhaust RAM when backends sort or hash at once", maxWorkMemPct,
			host.TotalBytes>>20))
	}
	return findings
}

// applyMemoryPressureGuard refuses raising any memory setting while the
// host's available memory is under minAvailablePct; lowering stays allowed.
func applyMemoryPressureGuard(findings []analyzer.Finding, host HostMemory,
	settings []collector.PGSetting) []analyzer.Finding {
	if host.TotalBytes <= 0 || host.AvailableBytes <= 0 ||
		host.AvailableBytes*100 >= host.TotalBytes*minAvailablePct {
		return findings
	}
	for i, f := range findings {
		name, v, ok := memorySetting(f)
		if !ok {
			continue
		}
		if current, known := currentBytes(settings, name); known && v <= current {
			continue
		}
		findings[i] = notExecutable(f, fmt.Sprintf("only %d MiB of %d MiB host memory is "+
			"available; raising %s now risks swapping or the OOM killer",
			host.AvailableBytes>>20, host.TotalBytes>>20, name))
	}
	return findings
}

func currentBytes(settings []collector.PGSetting, name string) (float64, bool) {
	for _, s := range settings {
		if s.Name != name {
			continue
		}
		v, err := pgconf.ParseValue(s.Setting, pgconf.GUCDoc{Unit: "bytes", BaseUnit: s.Unit})
		return v, err == nil
	}
	return 0, false
}
