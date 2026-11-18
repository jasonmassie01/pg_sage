package agentposture

import (
	"hash/fnv"
	"slices"

	"github.com/pg-sage/sidecar/internal/provider"
)

// Supabase Data API exposure (fleetpol-02c). A table anon can read is
// reachable from the internet only when the project's Data API serves its
// schema, which SQL cannot see; cloud telemetry reads it from the
// Management API. AP-03 and AP-04 findings on Supabase carry the state of
// their schema: known not served lowers a critical finding to warning,
// unknown stays critical (never inferred good). Off Supabase nothing is
// added.

// DataAPIExposure is the project's Data API exposure as last read.
type DataAPIExposure struct {
	Schemas []string // the schemas the Data API serves
}

// Data API states of a finding's schema (detail.data_api_exposed).
const (
	DataAPIExposed    = "true"
	DataAPINotExposed = "false"
	DataAPIUnknown    = "unknown"
)

func (p Platform) supabase() bool { return provider.Canonical(p.Provider) == provider.Supabase }

// dataAPIState is whether the Data API serves schema: "" off Supabase,
// unknown when it was not read (or the read listed nothing).
func (p Platform) dataAPIState(schema string) string {
	switch {
	case !p.supabase():
		return ""
	case p.DataAPI == nil || len(p.DataAPI.Schemas) == 0 || schema == "":
		return DataAPIUnknown
	case slices.Contains(p.DataAPI.Schemas, schema):
		return DataAPIExposed
	}
	return DataAPINotExposed
}

// withDataAPI adds the Data API state of schema to f and lowers a
// critical f to warning when the schema is known not served.
func withDataAPI(p Platform, schema string, f Finding) Finding {
	state := p.dataAPIState(schema)
	if state == "" {
		return f
	}
	f.DataAPIExposed = state
	switch state {
	case DataAPIExposed:
		f.Detail += " The project's Data API serves schema " + schema + ", so these " +
			"grants are reachable over the internet with the project's anon key."
	case DataAPINotExposed:
		if f.Severity == Critical {
			f.Severity = Warning
		}
		f.Detail += " The project's Data API does not serve schema " + schema + ", so " +
			"these grants are not reachable through it; any other path to these roles " +
			"still follows the grants."
	default:
		f.Detail += " Whether the project's Data API serves schema " + schema + " is " +
			"unknown (pg_sage reads it with SAGE_SUPABASE_OBSERVABILITY_TOKEN), so the " +
			"finding is treated as reachable."
	}
	return f
}

// dataAPIFingerprint folds the Data API state into the posture
// fingerprint, so a change of the state alone makes the next sweep run.
// It is 0 off Supabase; the order of the schemas does not matter.
func (p Platform) dataAPIFingerprint() int64 {
	if !p.supabase() {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte("data_api:"))
	if p.DataAPI == nil || len(p.DataAPI.Schemas) == 0 {
		_, _ = h.Write([]byte("unknown"))
		return int64(h.Sum64() | 1)
	}
	schemas := slices.Clone(p.DataAPI.Schemas)
	slices.Sort(schemas)
	for _, s := range slices.Compact(schemas) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	return int64(h.Sum64() | 1)
}
