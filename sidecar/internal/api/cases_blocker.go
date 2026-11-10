package api

import (
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
)

// incidentChainFromAny maps a decoded sage.incidents causal_chain (jsonb
// unmarshalled into any) to case chain links, keeping the structured
// root-blocker identity RCA stores on lock_contention links.
func incidentChainFromAny(v any) []cases.IncidentChainLink {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]cases.IncidentChainLink, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, cases.IncidentChainLink{
			Order:       int(floatValue(m["order"])),
			Signal:      stringValue(m["signal"]),
			Description: stringValue(m["description"]),
			Evidence:    stringValue(m["evidence"]),
			Blocker:     incidentBlockerFromAny(m["blocker"]),
		})
	}
	return out
}

// incidentBlockerFromAny decodes rca.BlockerIdentity JSON. A value that is
// not an object gives nil; unparseable times stay zero, which the
// playbook treats as an incomplete identity (blocked, never signalled).
func incidentBlockerFromAny(v any) *cases.IncidentBlocker {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return &cases.IncidentBlocker{
		PID:            int(floatValue(m["pid"])),
		BackendStart:   rfc3339TimeFromAny(m["backend_start"]),
		QueryStart:     rfc3339TimeFromAny(m["query_start"]),
		QuerySHA:       stringValue(m["query_sha"]),
		State:          stringValue(m["state"]),
		TotalBlocked:   int(floatValue(m["total_blocked"])),
		LockedRelation: stringValue(m["locked_relation"]),
		QueuedDDLPID:   int(floatValue(m["queued_ddl_pid"])),
	}
}

func rfc3339TimeFromAny(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, t)
		if err != nil {
			return time.Time{}
		}
		return parsed
	default:
		return time.Time{}
	}
}
