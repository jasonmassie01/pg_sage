package fleet

import (
	"context"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
)

// DatabaseManager manages multiple database instances.
type DatabaseManager struct {
	instances map[string]*DatabaseInstance
	cfg       *config.Config
	mu        sync.RWMutex
}

// NewManager creates a fleet manager from config.
func NewManager(cfg *config.Config) *DatabaseManager {
	return &DatabaseManager{
		instances: make(map[string]*DatabaseInstance),
		cfg:       cfg,
	}
}

// RegisterInstance adds a pre-built instance (used by main.go
// after connecting and creating components).
func (m *DatabaseManager) RegisterInstance(inst *DatabaseInstance) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.instances[inst.Name] = inst
}

// GetInstance returns a single instance by name.
func (m *DatabaseManager) GetInstance(name string) *DatabaseInstance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.instances[name]
}

// Instances returns all instances.
func (m *DatabaseManager) Instances() map[string]*DatabaseInstance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	// Return a copy to avoid holding lock
	cp := make(map[string]*DatabaseInstance, len(m.instances))
	for k, v := range m.instances {
		cp[k] = v
	}
	return cp
}

// Config returns the manager's config.
func (m *DatabaseManager) Config() *config.Config {
	return m.cfg
}

// FleetStatus returns the fleet overview with health scores.
func (m *DatabaseManager) FleetStatus() FleetOverview {
	m.mu.RLock()
	defer m.mu.RUnlock()

	overview := FleetOverview{
		Mode:      m.cfg.Mode,
		Databases: make([]DatabaseStatus, 0, len(m.instances)),
	}

	for _, inst := range m.instances {
		inst.Status.HealthScore = computeHealthScore(inst.Status)
		inst.Status.DatabaseName = inst.Name
		ds := DatabaseStatus{
			Name:   inst.Name,
			Tags:   inst.Config.Tags,
			Status: inst.Status,
		}
		overview.Databases = append(overview.Databases, ds)
	}

	// Sort by health score ascending (worst first).
	sort.Slice(overview.Databases, func(i, j int) bool {
		return overview.Databases[i].Status.HealthScore <
			overview.Databases[j].Status.HealthScore
	})

	// Build summary.
	for _, db := range overview.Databases {
		overview.Summary.TotalDatabases++
		if db.Status.Connected && db.Status.Error == "" {
			overview.Summary.Healthy++
		} else {
			overview.Summary.Degraded++
		}
		overview.Summary.TotalFindings += db.Status.FindingsOpen
		overview.Summary.TotalCritical += db.Status.FindingsCritical
		overview.Summary.TotalActions += db.Status.ActionsTotal
	}

	return overview
}

// computeHealthScore calculates 0-100 health for an instance.
func computeHealthScore(s *InstanceStatus) int {
	if !s.Connected {
		return 0
	}
	if s.Error != "" {
		return 0
	}
	score := 100
	score -= s.FindingsCritical * 25
	score -= s.FindingsWarning * 5
	if score < 0 {
		score = 0
	}
	return score
}

// EmergencyStop stops a specific database or all if name is empty.
func (m *DatabaseManager) EmergencyStop(name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	stopped := 0
	for n, inst := range m.instances {
		if name != "" && name != n {
			continue
		}
		if !inst.Stopped {
			inst.Stopped = true
			if inst.Pool != nil {
				ctx, cancel := context.WithTimeout(
					context.Background(), 5*time.Second,
				)
				_ = executor.SetEmergencyStop(ctx, inst.Pool, true)
				cancel()
			}
			if inst.cancel != nil {
				inst.cancel()
			}
			stopped++
			log.Printf("fleet: %s: emergency stop", n)
		}
	}
	return stopped
}

// Resume resumes a specific database or all if name is empty.
func (m *DatabaseManager) Resume(name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	resumed := 0
	for n, inst := range m.instances {
		if name != "" && name != n {
			continue
		}
		if inst.Stopped {
			inst.Stopped = false
			if inst.Pool != nil {
				ctx, cancel := context.WithTimeout(
					context.Background(), 5*time.Second,
				)
				_ = executor.SetEmergencyStop(ctx, inst.Pool, false)
				cancel()
			}
			resumed++
			log.Printf("fleet: %s: resumed", n)
		}
	}
	return resumed
}

// PoolForDatabase returns the connection pool for a named
// database, or the first available pool if name is empty
// or "all".
func (m *DatabaseManager) PoolForDatabase(
	name string,
) *pgxpool.Pool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if name != "" && name != "all" {
		if inst, ok := m.instances[name]; ok {
			return inst.Pool
		}
		return nil
	}
	for _, inst := range m.instances {
		return inst.Pool
	}
	return nil
}

// RemoveInstance removes an instance by name and closes its pool.
func (m *DatabaseManager) RemoveInstance(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, ok := m.instances[name]
	if !ok {
		return false
	}
	if inst.cancel != nil {
		inst.cancel()
	}
	if inst.Pool != nil {
		inst.Pool.Close()
	}
	delete(m.instances, name)
	return true
}

// InstanceCount returns the number of registered instances.
func (m *DatabaseManager) InstanceCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.instances)
}

// ResolveDatabaseName returns the actual database name for a filter
// value. If name is "all" or empty, it returns the name of the first
// registered instance (useful in standalone mode with a single DB).
// Returns the original name if no instances are registered.
func (m *DatabaseManager) ResolveDatabaseName(
	name string,
) string {
	if name != "" && name != "all" {
		return name
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for n := range m.instances {
		return n
	}
	return name
}
