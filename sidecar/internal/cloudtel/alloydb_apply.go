package cloudtel

import (
	"fmt"
	"time"
)

// alloyDBSample maps the newest AlloyDB values (keyed by metric without
// the prefix; nil = no fresh datapoint) onto a sample. Missing or invalid
// values stay nil with a reason, never zero.
func alloyDBSample(resource string, v map[string]*Point, now time.Time) Sample {
	s := Sample{Provider: "alloydb", Resource: resource, CollectedAt: now,
		StorageAutoGrows: true}
	var missing []string
	switch cpu := v["cpu/average_utilization"]; {
	case cpu == nil:
		missing = append(missing, "cpu: no datapoint")
	case !validPct(cpu.Value):
		missing = append(missing, fmt.Sprintf("cpu: invalid utilization %v", cpu.Value))
	default:
		s.CPUPct = cpu
	}
	vcpus := v["cpu/vcpus"]
	switch {
	case vcpus == nil:
		missing = append(missing, "memory_total: no vCPU count")
	case AlloyDBMemoryBytes(vcpus.Value) == 0:
		missing = append(missing, fmt.Sprintf("memory_total: no memory known for %v vCPUs",
			vcpus.Value))
	default:
		s.MemoryTotalBytes = &Point{Value: AlloyDBMemoryBytes(vcpus.Value), At: vcpus.At}
		s.MemoryTotalSource = MemorySourceMachineTier
	}
	if avail := v["memory/min_available_memory"]; avail != nil && avail.Value >= 0 {
		s.FreeableMemoryBytes = avail
	} else {
		missing = append(missing, "freeable_memory: no available-memory datapoint")
	}
	if c := v["postgres/total_connections"]; c != nil && c.Value >= 0 {
		s.Connections = c
	} else {
		missing = append(missing, "connections: no datapoint")
	}
	if lag := v["postgres/replication/maximum_lag"]; lag != nil && lag.Value >= 0 {
		s.ReplicaLagSeconds = &Point{Value: lag.Value / 1000, At: lag.At}
	} else {
		missing = append(missing, "replica_lag: no datapoint (no read pool, or none reported)")
	}
	s.Missing = missing
	return s
}

// validPct is a finite percentage in [0, 100].
func validPct(v float64) bool { return v >= 0 && v <= 100 }
