package cloudtel

import (
	"math"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/managedparam"
)

// Status is a database's telemetry as served to operators. Databases on
// one provider instance share its runtime: Statuses lists the instance
// once, Database is the first of its member Databases.
type Status struct {
	Database      string     `json:"database"`
	Databases     []string   `json:"databases,omitempty"`
	Provider      string     `json:"provider"`
	Cluster       string     `json:"cluster,omitempty"`       // Aurora
	InstanceRole  string     `json:"instance_role,omitempty"` // writer|reader|unknown
	Resource      string     `json:"resource,omitempty"`
	Available     bool       `json:"available"`
	Reason        string     `json:"reason,omitempty"`
	LastPollAt    *time.Time `json:"last_poll_at,omitempty"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	// NextPollAt is when the poller tries again (absent: not polling);
	// failed polls back off up to 10 minutes, 30 without credentials.
	NextPollAt          *time.Time           `json:"next_poll_at,omitempty"`
	ConsecutiveFailures int                  `json:"consecutive_failures"`
	Sample              *Sample              `json:"sample,omitempty"`
	Runway              Runway               `json:"storage_runway"`
	Withhold            []string             `json:"withhold"`
	Limits              Limits               `json:"limits"`
	Drift               []managedparam.Drift `json:"parameter_drift"`
}

// Status reports the runtime's state; Reason starts with "unavailable: "
// whenever no fresh sample backs the guards.
func (r *Runtime) Status() Status {
	now := r.now()
	provider, cluster, role := r.identity()
	r.mu.RLock()
	st := Status{Database: r.database, Provider: provider, Cluster: cluster,
		InstanceRole: role, Limits: r.limits,
		LastError: r.lastErr, Withhold: []string{}, ConsecutiveFailures: r.failures,
		Drift: append([]managedparam.Drift{}, r.drift...)}
	if !r.lastPoll.IsZero() {
		t := r.lastPoll
		st.LastPollAt = &t
	}
	if !r.nextPoll.IsZero() {
		t := r.nextPoll
		st.NextPollAt = &t
	}
	if !r.lastSuccess.IsZero() {
		t := r.lastSuccess
		st.LastSuccessAt = &t
	}
	var sample *Sample
	if r.sample != nil {
		c := r.sample.clone()
		sample = &c
	}
	history := r.history
	r.mu.RUnlock()
	switch {
	case r.reason != "":
		st.Reason = "unavailable: " + r.reason
	case sample == nil && st.LastError != "":
		st.Reason = "unavailable: " + st.LastError
	case sample == nil:
		st.Reason = "unavailable: waiting for the first sample"
	default:
		st.Available, st.Sample, st.Resource = true, sample, sample.Resource
		st.Runway = StorageRunway(history, now)
		st.Withhold = append(st.Withhold, Withhold(*sample, st.Runway, now, r.limits)...)
	}
	if math.IsInf(st.Runway.Hours, 1) {
		st.Runway.Hours = -1 // JSON has no infinity: -1 means "not running out"
	}
	return st
}

// clusterMember is a source that knows its instance's cluster and role.
type clusterMember interface {
	ClusterMember() (cluster, role string)
}

// identity is read from the source on each call: an instance endpoint
// learns that it is Aurora, and its role, only once AWS has answered.
func (r *Runtime) identity() (provider, cluster, role string) {
	if r.src == nil {
		return r.provider, "", ""
	}
	if m, ok := r.src.(clusterMember); ok {
		cluster, role = m.ClusterMember()
	}
	return r.src.Provider(), cluster, role
}

// StatusSource is anything that reports a database's host telemetry
// status: a Runtime, or a status-only adapter such as Supabase's.
type StatusSource interface {
	Database() string
	Status() Status
}

// registry maps each member database to its status source; databases on
// one provider instance map to the same shared runtime.
var registry = struct {
	sync.RWMutex
	m map[string]StatusSource
}{m: map[string]StatusSource{}}

// isNil reports a nil source, including a typed nil pointer.
func isNil(s StatusSource) bool {
	if s == nil {
		return true
	}
	v := reflect.ValueOf(s)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

// Register publishes a database's status source to the API.
func Register(s StatusSource) {
	if isNil(s) {
		return
	}
	RegisterMember(s.Database(), s)
}

// Unregister removes s if it is still the registered source.
func Unregister(s StatusSource) {
	if isNil(s) {
		return
	}
	UnregisterMember(s.Database(), s)
}

// RegisterMember publishes s as database's source; databases that share
// one instance's runtime each register it under their own name.
func RegisterMember(database string, s StatusSource) {
	if database == "" || isNil(s) {
		return
	}
	registry.Lock()
	defer registry.Unlock()
	registry.m[database] = s
}

// UnregisterMember removes database's source if it is still s.
func UnregisterMember(database string, s StatusSource) {
	if isNil(s) {
		return
	}
	registry.Lock()
	defer registry.Unlock()
	if registry.m[database] == s {
		delete(registry.m, database)
	}
}

// Lookup returns the database's runtime (nil: none, or a status-only
// source, which has no guards, backup posture or resource identity).
func Lookup(database string) *Runtime {
	registry.RLock()
	defer registry.RUnlock()
	rt, _ := registry.m[database].(*Runtime)
	return rt
}

// sharedSource is one registered source and the databases it serves.
type sharedSource struct {
	src     StatusSource
	members []string
}

// Statuses reports every registered source once, sorted by database: a
// runtime shared by several databases is one status naming them all.
func Statuses() []Status {
	registry.RLock()
	groups := make([]*sharedSource, 0, len(registry.m))
	bySource := map[StatusSource]*sharedSource{}
	for db, s := range registry.m {
		g := &sharedSource{src: s, members: []string{db}}
		if !reflect.TypeOf(s).Comparable() { // a map key would panic
			groups = append(groups, g)
			continue
		}
		if shared := bySource[s]; shared != nil {
			shared.members = append(shared.members, db)
			continue
		}
		groups, bySource[s] = append(groups, g), g
	}
	registry.RUnlock()
	out := make([]Status, 0, len(groups))
	for _, g := range groups {
		sort.Strings(g.members)
		st := g.src.Status()
		st.Database, st.Databases = g.members[0], g.members
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Database < out[j].Database })
	return out
}
