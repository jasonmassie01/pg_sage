package cloudtel

import (
	"fmt"
	"time"
)

// azureReadings are one collection's newest Azure values (nil = no fresh
// datapoint): the server's metrics, the worst replica lag, the SKU and
// whether storage auto-grows.
type azureReadings struct {
	sku      string
	autoGrow bool
	primary  map[string]*Point
	lag      *Point
	replicas int
}

// sample maps the readings; missing or invalid values stay nil with a
// reason, never zero.
func (r azureReadings) sample(resource string, now time.Time) Sample {
	s := Sample{Provider: "azure", Resource: resource, CollectedAt: now,
		StorageAutoGrows: r.autoGrow}
	p := r.primary
	var missing []string
	switch cpu := p["cpu_percent"]; {
	case cpu == nil:
		missing = append(missing, "cpu: no datapoint")
	case !validPct(cpu.Value):
		missing = append(missing, fmt.Sprintf("cpu: invalid utilization %v", cpu.Value))
	default:
		s.CPUPct = cpu
	}
	missing = append(missing, r.applyMemory(&s, now)...)
	used, free := p["storage_used"], p["storage_free"]
	if used != nil && free != nil && used.Value >= 0 && free.Value >= 0 {
		capacity := &Point{Value: used.Value + free.Value, At: olderOf(used, free)}
		s.FreeStorageBytes, s.StorageCapacityBytes = free, capacity
		s.AllocatedStorageBytes = capacity
	} else {
		missing = append(missing, "free_storage: no storage used and free datapoints")
	}
	s.Connections, s.ReadIOPS, s.WriteIOPS = nonNegative(p["active_connections"]),
		nonNegative(p["read_iops"]), nonNegative(p["write_iops"])
	if s.Connections == nil {
		missing = append(missing, "connections: no datapoint")
	}
	if s.ReadIOPS == nil || s.WriteIOPS == nil {
		missing = append(missing, "iops: no read and write IOPS datapoints")
	}
	s.ReplicaLagSeconds = nonNegative(r.lag)
	if r.replicas > 0 && s.ReplicaLagSeconds == nil {
		missing = append(missing, "replica_lag: no datapoint from the replicas")
	}
	s.Missing = missing
	return s
}

// applyMemory sets the SKU's memory and, from memory_percent, the
// available memory; it returns what is missing.
func (r azureReadings) applyMemory(s *Sample, now time.Time) []string {
	total := AzureSKUMemoryBytes(r.sku)
	if total == 0 {
		return []string{"memory_total: unknown SKU " + sanitize(r.sku),
			"freeable_memory: needs the memory total"}
	}
	s.MemoryTotalBytes = &Point{Value: total, At: now}
	s.MemoryTotalSource = MemorySourceInstanceClass
	pct := r.primary["memory_percent"]
	if pct == nil || !validPct(pct.Value) {
		return []string{"freeable_memory: no valid memory_percent datapoint"}
	}
	s.FreeableMemoryBytes = &Point{Value: total * (100 - pct.Value) / 100, At: pct.At}
	return nil
}

func nonNegative(p *Point) *Point {
	if p == nil || !(p.Value >= 0) {
		return nil
	}
	return p
}
