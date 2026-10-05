package cloudtel

import (
	"fmt"
	"strings"
	"time"
)

type monitoringValues struct {
	primary map[string]*Point
	conns   *Point
	lag     *Point
}

// collectMonitoring reduces the series to the primary's newest values,
// the sum of its backends over all databases and the replicas' max lag.
func collectMonitoring(series []tsSeries, primary string,
	now time.Time) (monitoringValues, error) {
	v := monitoringValues{primary: map[string]*Point{}}
	for _, ts := range series {
		p, interval, err := newestSeriesPoint(ts, now)
		if err != nil {
			return v, err
		}
		if p == nil || p.Value < 0 {
			continue
		}
		metric := strings.TrimPrefix(ts.Metric.Type, cloudSQLMetric)
		db := ts.Resource.Labels["database_id"]
		switch {
		case metric == "replication/replica_lag":
			if db != primary && (v.lag == nil || p.Value > v.lag.Value) {
				v.lag = p
			}
		case db != primary:
		case metric == "postgresql/num_backends":
			if v.conns == nil {
				v.conns = &Point{At: p.At}
			}
			v.conns.Value += p.Value
			if p.At.After(v.conns.At) {
				v.conns.At = p.At
			}
		case metric == "disk/read_ops_count" || metric == "disk/write_ops_count":
			v.primary[metric] = &Point{Value: p.Value / interval, At: p.At}
		default:
			v.primary[metric] = p
		}
	}
	return v, nil
}

func olderOf(a, b *Point) time.Time {
	if a.At.Before(b.At) {
		return a.At
	}
	return b.At
}

// applyMonitoring maps Cloud Monitoring values onto the sample.
func applyMonitoring(s *Sample, series []tsSeries, primary string, hasReplicas bool,
	now time.Time) error {
	v, err := collectMonitoring(series, primary, now)
	if err != nil {
		return err
	}
	p := v.primary
	var missing []string
	if cpu := p["cpu/utilization"]; cpu == nil {
		missing = append(missing, "cpu: no datapoint")
	} else if pct := cpu.Value * 100; pct > 100 {
		missing = append(missing, fmt.Sprintf("cpu: invalid utilization %v", cpu.Value))
	} else {
		s.CPUPct = &Point{Value: pct, At: cpu.At}
	}
	quota, usage := p["memory/quota"], p["memory/usage"]
	if quota != nil && quota.Value > 0 {
		s.MemoryTotalBytes, s.MemoryTotalSource = quota, MemorySourceCloudMonitoring
	}
	if quota != nil && usage != nil && usage.Value <= quota.Value {
		s.FreeableMemoryBytes = &Point{Value: quota.Value - usage.Value, At: olderOf(quota, usage)}
	} else {
		missing = append(missing, "freeable_memory: no memory quota and usage")
	}
	disk, used := p["disk/quota"], p["disk/bytes_used"]
	if disk != nil {
		s.AllocatedStorageBytes = disk
	}
	if disk != nil && used != nil && used.Value <= disk.Value {
		s.FreeStorageBytes = &Point{Value: disk.Value - used.Value, At: olderOf(disk, used)}
	} else {
		missing = append(missing, "free_storage: no disk quota and usage")
	}
	s.ReadIOPS, s.WriteIOPS = p["disk/read_ops_count"], p["disk/write_ops_count"]
	if s.ReadIOPS == nil || s.WriteIOPS == nil {
		missing = append(missing, "iops: no disk operation counts")
	}
	s.Connections = v.conns
	if v.conns == nil {
		missing = append(missing, "connections: no backend counts")
	}
	s.ReplicaLagSeconds = v.lag
	if hasReplicas && v.lag == nil {
		missing = append(missing, "replica_lag: no datapoint from the replicas")
	}
	s.Missing = append(s.Missing, missing...)
	return nil
}
