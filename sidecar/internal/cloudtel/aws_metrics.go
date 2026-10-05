package cloudtel

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/managedparam"
)

// cwField maps one CloudWatch query to a sample field.
type cwField struct {
	name  string // the Missing label
	scale float64
	set   func(*Sample, *Point)
	lag   bool // replica lag: the maximum over the queries wins
	pct   bool // a percentage (at most 100)
}

func cloudWatchQueries(inst rdsInstance, aurora bool) ([]cwQuery, map[string]cwField) {
	const dim = "DBInstanceIdentifier"
	fields := map[string]cwField{
		"cpu": {name: "cpu", scale: 1, pct: true,
			set: func(s *Sample, p *Point) { s.CPUPct = p }},
		"freeable": {name: "freeable_memory", scale: 1,
			set: func(s *Sample, p *Point) { s.FreeableMemoryBytes = p }},
		"readiops": {name: "read_iops", scale: 1,
			set: func(s *Sample, p *Point) { s.ReadIOPS = p }},
		"writeiops": {name: "write_iops", scale: 1,
			set: func(s *Sample, p *Point) { s.WriteIOPS = p }},
		"readtp": {name: "read_throughput", scale: 1,
			set: func(s *Sample, p *Point) { s.ReadBytesPerSec = p }},
		"writetp": {name: "write_throughput", scale: 1,
			set: func(s *Sample, p *Point) { s.WriteBytesPerSec = p }},
		"conns": {name: "connections", scale: 1,
			set: func(s *Sample, p *Point) { s.Connections = p }},
	}
	metric := map[string]string{"cpu": "CPUUtilization", "freeable": "FreeableMemory",
		"readiops": "ReadIOPS", "writeiops": "WriteIOPS", "readtp": "ReadThroughput",
		"writetp": "WriteThroughput", "conns": "DatabaseConnections"}
	setLag := func(s *Sample, p *Point) { s.ReplicaLagSeconds = p }
	if aurora {
		metric["auroralag"] = "AuroraReplicaLagMaximum"
		fields["auroralag"] = cwField{name: "replica_lag", scale: 0.001, lag: true, set: setLag}
	} else {
		metric["freestorage"] = "FreeStorageSpace"
		fields["freestorage"] = cwField{name: "free_storage", scale: 1,
			set: func(s *Sample, p *Point) { s.FreeStorageBytes = p }}
	}
	var queries []cwQuery
	for _, id := range sortedKeys(metric) {
		queries = append(queries, cwQuery{id: id, metric: metric[id], dimension: dim,
			value: inst.ID})
	}
	if !aurora {
		for i, replica := range inst.Replicas {
			if i >= 20 || !ValidRDSIdentifier(replica) {
				continue
			}
			id := fmt.Sprintf("lag%d", i)
			queries = append(queries, cwQuery{id: id, metric: "ReplicaLag", dimension: dim,
				value: replica})
			fields[id] = cwField{name: "replica_lag", scale: 1, lag: true, set: setLag}
		}
	}
	return queries, fields
}

// applyCloudWatch sets each field from its newest fresh datapoint; absent
// and invalid values are reported in Missing.
func applyCloudWatch(s *Sample, points map[string][]Point, fields map[string]cwField,
	now time.Time) error {
	missing := map[string]string{}
	var lag *Point
	for _, id := range sortedKeys(fields) {
		f := fields[id]
		p, err := newestPoint(points[id], now)
		if err != nil {
			return fmt.Errorf("CloudWatch %s: %w", f.name, err)
		}
		if p == nil {
			missing[f.name] = "no datapoint in the last " + MaxPointAge.String()
			continue
		}
		v := p.Value * f.scale
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || (f.pct && v > 100) {
			missing[f.name] = fmt.Sprintf("invalid value %v", p.Value)
			continue
		}
		scaled := &Point{Value: v, At: p.At}
		if f.lag {
			if lag == nil || scaled.Value > lag.Value {
				lag = scaled
			}
			continue
		}
		f.set(s, scaled)
	}
	if lag != nil {
		s.ReplicaLagSeconds = lag
		delete(missing, "replica_lag")
	}
	for _, name := range sortedKeys(missing) {
		s.Missing = append(s.Missing, name+": "+missing[name])
	}
	return nil
}

// Target resolves the parameter group behind the instance and reads its
// parameters (managedparam.Resolver).
func (s *AWSSource) Target(ctx context.Context) (managedparam.Target, error) {
	inst, err := s.resolve(ctx)
	if err != nil {
		return managedparam.Target{}, err
	}
	t := managedparam.Target{Provider: s.Provider(), Region: s.opts.Region,
		InstanceID: inst.ID, ParameterGroup: inst.ParameterGroup,
		ParameterGroupIsDefault: strings.HasPrefix(inst.ParameterGroup, "default."),
		ParameterGroupStatus:    inst.ParameterApplyStatus}
	if inst.ParameterGroup == "" {
		return managedparam.Target{}, fmt.Errorf("%w: instance %s has no DB parameter group",
			ErrIdentity, inst.ID)
	}
	params, err := s.rds.parameters(ctx, inst.ParameterGroup)
	if err != nil {
		return managedparam.Target{}, err
	}
	t.Params, t.Known = params, true
	return t, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
