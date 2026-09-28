package fleet

import (
	"sort"

	"github.com/pg-sage/sidecar/internal/value"
)

// ValueSources lists every registered instance as a value ledger source,
// in name order. An instance without a pool (failed to connect) is kept so
// value readers report it as unavailable instead of silently dropping it.
func ValueSources(m *DatabaseManager) []value.Source {
	if m == nil {
		return nil
	}
	instances := m.Instances()
	sources := make([]value.Source, 0, len(instances))
	for name, inst := range instances {
		source := value.Source{Name: name}
		if inst != nil {
			source.Pool = inst.Pool
		}
		sources = append(sources, source)
	}
	sort.Slice(sources, func(i, j int) bool {
		return sources[i].Name < sources[j].Name
	})
	return sources
}
