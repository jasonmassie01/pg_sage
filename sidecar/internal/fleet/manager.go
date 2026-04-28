package fleet

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
)

var ErrDatabaseNotFound = errors.New("database not found")

// DatabaseManager manages multiple database instances.
type DatabaseManager struct {
	instances   map[string]*DatabaseInstance
	cfg         *config.Config
	primaryName string // first registered instance name
	mu          sync.RWMutex
}

// NewManager creates a fleet manager from config.
func NewManager(cfg *config.Config) *DatabaseManager {
	return &DatabaseManager{
		instances: make(map[string]*DatabaseInstance),
		cfg:       cfg,
	}
}

// RegisterInstance adds a pre-built instance (used by main.go
// after connecting and creating components). The first registered
// instance becomes the primary — used for auth and session storage.
func (m *DatabaseManager) RegisterInstance(inst *DatabaseInstance) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.primaryName == "" {
		m.primaryName = inst.Name
	}
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

	anyStopped := false
	for _, inst := range m.instances {
		// Snapshot fields under the instance lock so that background
		// writers (updateInstanceFindings, config_handlers) don't race
		// with the health-score compute or the JSON marshaller.
		snap := inst.SnapshotStatus()
		snap = EnsureCapabilities(m.cfg, inst, snap, time.Now().UTC())
		snap.HealthScore = computeHealthScore(snap)
		snap.DatabaseName = inst.Name
		ds := DatabaseStatus{
			ID:         inst.DatabaseID,
			DatabaseID: inst.DatabaseID,
			Name:       inst.Name,
			Tags:       inst.Config.Tags,
			Status:     snap,
		}
		overview.Databases = append(overview.Databases, ds)
		if inst.Stopped {
			anyStopped = true
		}
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
	overview.Summary.EmergencyStopped = anyStopped

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

// RecordHealthSnapshots appends the current health score + finding
// counts for every live instance to its sage.health_history. Intended
// to be called on the same cadence as FleetStatus is consumed so the
// Overview time-series stays populated even when scores are flat.
// A write failure on one instance does not abort the rest.
func (m *DatabaseManager) RecordHealthSnapshots(ctx context.Context) {
	m.mu.RLock()
	// Copy (name,pool,status-snapshot) tuples under the read lock,
	// then release it before doing network I/O.
	type sample struct {
		name string
		pool *pgxpool.Pool
		s    *InstanceStatus
	}
	samples := make([]sample, 0, len(m.instances))
	for _, inst := range m.instances {
		if inst.Pool == nil {
			continue
		}
		snap := inst.SnapshotStatus()
		snap.HealthScore = computeHealthScore(snap)
		samples = append(samples, sample{
			name: inst.Name, pool: inst.Pool, s: snap,
		})
	}
	m.mu.RUnlock()

	for _, x := range samples {
		qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, err := x.pool.Exec(qctx,
			`INSERT INTO sage.health_history
			 (database_name, health_score, findings_open,
			  findings_critical, findings_warning,
			  findings_info, actions_total)
			 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			x.name, x.s.HealthScore, x.s.FindingsOpen,
			x.s.FindingsCritical, x.s.FindingsWarning,
			x.s.FindingsInfo, x.s.ActionsTotal,
		)
		cancel()
		if err != nil {
			log.Printf("fleet: %s: health_history write failed: %v",
				x.name, err)
		}
	}
}

// EmergencyStop blocks action execution for a specific database, or all
// databases if name is empty. Monitoring goroutines intentionally keep
// running so Resume can clear the guard without needing to reconstruct
// per-instance collectors/analyzers/orchestrators.
func (m *DatabaseManager) EmergencyStop(name string) int {
	stopped, _ := m.EmergencyStopStrict(name)
	return stopped
}

func (m *DatabaseManager) EmergencyStopStrict(name string) (int, error) {
	return m.setEmergencyStopped(name, true)
}

func (m *DatabaseManager) setEmergencyStopped(
	name string,
	stopped bool,
) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if name != "" {
		if _, ok := m.instances[name]; !ok {
			return 0, fmt.Errorf("%w: %s", ErrDatabaseNotFound, name)
		}
	}
	changed := 0
	for n, inst := range m.instances {
		if name != "" && name != n {
			continue
		}
		if inst.Stopped != stopped {
			if inst.Pool != nil {
				ctx, cancel := context.WithTimeout(
					context.Background(), 5*time.Second,
				)
				err := executor.SetEmergencyStop(ctx, inst.Pool, stopped)
				cancel()
				if err != nil {
					return changed, fmt.Errorf(
						"persisting emergency stop for %s: %w", n, err)
				}
			}
			inst.Stopped = stopped
			changed++
			if stopped {
				log.Printf("fleet: %s: emergency stop", n)
			} else {
				log.Printf("fleet: %s: resumed", n)
			}
		}
	}
	return changed, nil
}

// Resume resumes a specific database or all if name is empty.
func (m *DatabaseManager) Resume(name string) int {
	resumed, _ := m.ResumeStrict(name)
	return resumed
}

func (m *DatabaseManager) ResumeStrict(name string) (int, error) {
	return m.setEmergencyStopped(name, false)
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
	// Return the primary instance's pool (deterministic).
	if m.primaryName != "" {
		if inst, ok := m.instances[m.primaryName]; ok {
			return inst.Pool
		}
	}
	return nil
}

// AllPools returns the connection pools for all connected
// instances. Used for operations that must search across
// all databases (e.g. finding detail, suppress/unsuppress).
func (m *DatabaseManager) AllPools() []*pgxpool.Pool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var pools []*pgxpool.Pool
	for _, inst := range m.instances {
		if inst.Pool != nil {
			pools = append(pools, inst.Pool)
		}
	}
	return pools
}

// RemoveInstance removes an instance by name and closes its pool.
func (m *DatabaseManager) RemoveInstance(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, ok := m.instances[name]
	if !ok {
		return false
	}
	if inst.Cancel != nil {
		inst.Cancel()
	}
	if inst.Pool != nil {
		inst.Pool.Close()
	}
	delete(m.instances, name)
	return true
}

// GetInstanceByDatabaseID returns the instance with the given
// sage.databases ID, or nil if not found.
func (m *DatabaseManager) GetInstanceByDatabaseID(
	id int,
) *DatabaseInstance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, inst := range m.instances {
		if inst.DatabaseID == id {
			return inst
		}
	}
	return nil
}

// UpdateInstanceMetadata updates non-connection runtime metadata for
// an existing instance without closing its pool. This is used by
// YAML fleet mode where the primary pool is also captured for auth
// and catalog storage, so closing it during a UI edit can break the
// API server itself.
func (m *DatabaseManager) UpdateInstanceMetadata(
	oldName, newName string,
	cfg config.DatabaseConfig,
	databaseID int,
	trustLevel string,
) bool {
	m.mu.Lock()
	inst, ok := m.instances[oldName]
	if !ok {
		m.mu.Unlock()
		return false
	}
	if oldName != newName {
		delete(m.instances, oldName)
		m.instances[newName] = inst
		if m.primaryName == oldName {
			m.primaryName = newName
		}
	}
	inst.Name = newName
	inst.DatabaseID = databaseID
	inst.Config = cfg
	m.mu.Unlock()

	inst.UpdateStatus(func(s *InstanceStatus) {
		s.TrustLevel = trustLevel
		s.DatabaseName = newName
	})
	return true
}

// InstanceCount returns the number of registered instances.
func (m *DatabaseManager) InstanceCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.instances)
}

// ResolveDatabaseName returns the concrete database name for a filter
// value when the request targets a single database. Fleet-wide scope
// stays "all" when more than one instance is registered.
func (m *DatabaseManager) ResolveDatabaseName(
	name string,
) string {
	if name != "" && name != "all" {
		return name
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.instances) > 1 {
		return "all"
	}
	if m.primaryName != "" {
		if _, ok := m.instances[m.primaryName]; ok {
			return m.primaryName
		}
	}
	for n := range m.instances {
		return n
	}
	return name
}
