package fleet

import (
	"context"
	"errors"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
)

var (
	ErrDatabaseNotFound = errors.New("database not found")
	ErrInvalidInstance  = errors.New("invalid database instance")
	ErrInstanceConflict = errors.New("database instance changed")
)

const defaultInstanceTeardownTimeout = 30 * time.Second

// DatabaseManager manages multiple database instances.
type DatabaseManager struct {
	instances   map[string]*DatabaseInstance
	cfg         *config.Config
	primaryName string // first registered instance name
	// observeOnly are the observe-only member names (fleetpol-01d), guarded
	// by mu: their actions are refused (ActionTarget).
	observeOnly map[string]bool
	// census lists the sprawl census's standing proposals (fleetpol-03),
	// guarded by mu; nil outside a meta-db fleet.
	census    CensusProposer
	mu        sync.RWMutex
	lifecycle chan struct{}
	// persistStop writes the durable emergency_stop flag as an actor. nil
	// uses persistEmergencyStop; tests inject failures here.
	persistStop func(context.Context, *DatabaseInstance, bool, string) error
	// readStop reads the durable flag when a runtime is published. nil uses
	// readPersistedStop; tests inject states and errors here.
	readStop func(context.Context, *DatabaseInstance) (executor.EmergencyStopState, error)
	// transitionMu orders persisted emergency-stop transitions so that
	// concurrent stops and resumes end with memory matching sage.config.
	transitionMu sync.Mutex
	// fleetStop is the fleet-wide stop latch (fleet-09), guarded by mu;
	// fleetStore keeps it in the meta database (nil outside meta mode).
	fleetStop  fleetStopState
	fleetStore FleetStopStore
	// readScope and persistScope read and write a member's
	// emergency_stop_scope row; cancelInFlight halts a stopped database's
	// running actions. nil uses the defaults; tests inject here.
	readScope      func(context.Context, *DatabaseInstance) (bool, error)
	persistScope   func(context.Context, *DatabaseInstance, bool, string) error
	cancelInFlight func(*DatabaseInstance) ([]int64, error)
	// budget reports the fleet LLM budget per database (fleet-06); nil when
	// no fleet budget is configured. Guarded by mu.
	budget BudgetSource
	// readOnly marks a ReadView (apiauth-08b): every mutation is refused.
	readOnly bool
}

// NewManager creates a fleet manager from config.
func NewManager(cfg *config.Config) *DatabaseManager {
	m := &DatabaseManager{
		instances: make(map[string]*DatabaseInstance),
		cfg:       cfg,
		lifecycle: make(chan struct{}, 1),
	}
	m.lifecycle <- struct{}{}
	return m
}

// RegisterInstance adds a pre-built instance (used by main.go
// after connecting and creating components). The first *connected*
// instance becomes the primary — used for auth and session storage.
// Failed instances (nil Pool) are still registered so their error
// surfaces in fleet status, but they must never become primary: a
// nil-pool primary breaks auth-route registration and every
// "all"-scoped query (see PoolForDatabase).
func (m *DatabaseManager) RegisterInstance(inst *DatabaseInstance) {
	if m.readOnly {
		return
	}
	var retired *DatabaseInstance
	var pending []*DatabaseInstance
	m.restorePersistedStop(inst)
	_ = m.WithLifecycle(context.Background(), func(*LifecycleMutation) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		retired = m.instances[inst.Name]
		inherited := inheritEmergencyStop(retired, inst)
		if m.primaryName == "" && inst.Pool != nil {
			m.primaryName = inst.Name
		}
		stampRegistered(inst, time.Now())
		m.instances[inst.Name] = inst
		pending = m.applyFleetLatchLocked(inst, inherited)
		return nil
	})
	m.persistInheritedStops(pending)
	if retired != nil && retired != inst {
		go func() { _ = cleanupRejectedCandidate(retired) }()
	}
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

// InstanceConfigs returns a copy of each instance's per-database config,
// read under the manager lock: UpdateMetadata swaps Config in place, so
// callers must not read inst.Config through the Instances() pointers.
func (m *DatabaseManager) InstanceConfigs() map[string]config.DatabaseConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]config.DatabaseConfig, len(m.instances))
	for name, inst := range m.instances {
		if inst == nil {
			continue
		}
		cfg := inst.Config
		cfg.Tags = cloneTags(inst.Config.Tags)
		out[name] = cfg
	}
	return out
}

// cloneTags copies a tag list so callers never share its backing array.
func cloneTags(tags []string) []string {
	if tags == nil {
		return nil
	}
	out := make([]string, len(tags))
	copy(out, tags)
	return out
}

// Config returns the manager's config.
func (m *DatabaseManager) Config() *config.Config {
	return m.cfg
}

// FleetStatus returns the fleet overview with health scores, worst first
// and by name among equal scores.
func (m *DatabaseManager) FleetStatus() FleetOverview {
	m.mu.RLock()
	defer m.mu.RUnlock()

	overview := FleetOverview{
		Mode:      m.cfg.Mode,
		Databases: make([]DatabaseStatus, 0, len(m.instances)),
	}
	now := time.Now()
	budgets := m.budgetSnapshotLocked()
	for _, inst := range m.instances {
		overview.Databases = append(overview.Databases,
			m.databaseStatusLocked(inst, now, budgets))
		if inst.Stopped {
			overview.Summary.EmergencyStoppedCount++
		}
	}

	applyFleetStopStatus(&overview.Summary, m.fleetStop)

	dbs := overview.Databases
	sort.SliceStable(dbs, func(i, j int) bool {
		if dbs[i].Status.HealthScore != dbs[j].Status.HealthScore {
			return dbs[i].Status.HealthScore < dbs[j].Status.HealthScore
		}
		return dbs[i].Name < dbs[j].Name
	})

	summarizeFleet(&overview)
	return overview
}

// summarizeFleet fills the overview's totals from its database rows. A
// database is healthy when it is connected, has no error and scores at
// least healthyScoreFloor; every other database is degraded.
func summarizeFleet(overview *FleetOverview) {
	for _, db := range overview.Databases {
		overview.Summary.TotalDatabases++
		if db.Status.Connected && db.Status.Error == "" &&
			db.Status.HealthScore >= healthyScoreFloor {
			overview.Summary.Healthy++
		} else {
			overview.Summary.Degraded++
		}
		overview.Summary.TotalFindings += db.Status.FindingsOpen
		overview.Summary.TotalCritical += db.Status.FindingsCritical
		overview.Summary.TotalActions += db.Status.ActionsTotal
		addCaseCounts(&overview.Summary, db.Status)
	}
	overview.Summary.EmergencyStopped = overview.Summary.EmergencyStoppedCount > 0 ||
		overview.Summary.FleetStopped
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
	now := time.Now()
	for _, inst := range m.instances {
		if inst.Pool == nil {
			continue
		}
		snap := inst.SnapshotStatus()
		m.scoreLocked(inst, snap, now)
		samples = append(samples, sample{
			name: inst.Name, pool: inst.Pool, s: snap,
		})
	}
	m.mu.RUnlock()

	for _, x := range samples {
		recordHealthSample(ctx, x.name, x.pool, x.s)
	}
}

// RecordHealthSnapshot persists one instance's current fleet health. Per-DB
// orchestrators use this instead of recording the entire fleet on every tick.
func (m *DatabaseManager) RecordHealthSnapshot(
	ctx context.Context, name string,
) {
	m.mu.RLock()
	inst := m.instances[name]
	if inst == nil || inst.Pool == nil {
		m.mu.RUnlock()
		return
	}
	pool := inst.Pool
	snapshot := inst.SnapshotStatus()
	m.scoreLocked(inst, snapshot, time.Now())
	m.mu.RUnlock()
	recordHealthSample(ctx, name, pool, snapshot)
}

func recordHealthSample(
	ctx context.Context,
	name string,
	pool *pgxpool.Pool,
	snapshot *InstanceStatus,
) {
	qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := pool.Exec(qctx,
		`/* pg_sage */ INSERT INTO sage.health_history
		 (database_name, health_score, findings_open,
		  findings_critical, findings_warning,
		  findings_info, actions_total)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		name, snapshot.HealthScore, snapshot.FindingsOpen,
		snapshot.FindingsCritical, snapshot.FindingsWarning,
		snapshot.FindingsInfo, snapshot.ActionsTotal,
	)
	if err != nil {
		log.Printf("fleet: %s: health_history write failed: %v", name, err)
	}
}

// EmergencyStop blocks action execution for a specific database, or all
// databases if name is empty. Monitoring goroutines intentionally keep
// running so Resume can clear the guard without needing to reconstruct
// per-instance collectors/analyzers/orchestrators. Every target is stopped
// in memory even when its persisted flag cannot be written.
func (m *DatabaseManager) EmergencyStop(name string) int {
	stopped, _ := m.EmergencyStopStrict(name, executor.EmergencyStopActorSystem)
	return stopped
}

func (m *DatabaseManager) EmergencyStopStrict(name, actor string) (int, error) {
	result, err := m.setEmergencyStopped(name, true, actor)
	return result.Changed, err
}

// Resume resumes a specific database or all if name is empty.
func (m *DatabaseManager) Resume(name string) int {
	resumed, _ := m.ResumeStrict(name, executor.EmergencyStopActorSystem)
	return resumed
}

// ResumeStrict resumes one database, or all when name is empty. A
// per-database resume while a stop of all holds returns ErrFleetStopped.
func (m *DatabaseManager) ResumeStrict(name, actor string) (int, error) {
	result, err := m.setEmergencyStopped(name, false, actor)
	return result.Changed, err
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
		if inst, ok := m.instances[m.primaryName]; ok && inst.Pool != nil {
			return inst.Pool
		}
	}
	// Fallback: the primary may be unset or its pool gone (e.g. the
	// primary was removed). Return the first connected instance in
	// deterministic name order so auth/"all"-scoped queries still work.
	return m.firstConnectedPoolLocked()
}

// firstConnectedPoolLocked returns the pool of the first connected
// instance in sorted name order, or nil if none are connected. The
// caller must hold m.mu (read or write).
func (m *DatabaseManager) firstConnectedPoolLocked() *pgxpool.Pool {
	names := make([]string, 0, len(m.instances))
	for n := range m.instances {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if inst := m.instances[n]; inst != nil && inst.Pool != nil {
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
	updated := false
	_ = m.WithLifecycle(context.Background(), func(*LifecycleMutation) error {
		m.mu.Lock()
		inst, ok := m.instances[oldName]
		if !ok || (oldName != newName && m.instances[newName] != nil) {
			m.mu.Unlock()
			return nil
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
		inst.setConfig(cfg)
		m.mu.Unlock()
		inst.UpdateStatus(func(s *InstanceStatus) {
			s.TrustLevel = trustLevel
			s.DatabaseName = newName
		})
		updated = true
		return nil
	})
	return updated
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
