package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/pg-sage/sidecar/internal/azure"
	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/managedparam"
)

// clouds-08: the databases a process monitors on one provider instance
// share one telemetry runtime, so the instance is polled (and billed)
// once per interval, not once per database. The share owns the runtime:
// the first database to acquire it builds it and starts polling, the last
// to release it stops polling and unregisters it.

// cloudBuild builds a runtime and its managed-change resolver (nil, nil:
// the provider has no telemetry or it is disabled).
type cloudBuild func(ctx context.Context) (*cloudtel.Runtime, managedparam.Resolver)

type cloudShareEntry struct {
	rt       *cloudtel.Runtime
	resolver managedparam.Resolver
	members  map[string]int // database -> acquisitions (runtime generations)
	refs     int
	cancel   context.CancelFunc
	done     chan struct{}
	// prev is the entry a reload replaced while the old generation still
	// holds it; a discarded reload restores it.
	prev *cloudShareEntry
}

// cloudTelemetryShare holds the shared runtimes by provider resource key.
type cloudTelemetryShare struct {
	mu      sync.Mutex
	entries map[string]*cloudShareEntry
}

func newCloudTelemetryShare() *cloudTelemetryShare {
	return &cloudTelemetryShare{entries: map[string]*cloudShareEntry{}}
}

// processCloudTelemetry is the process's share.
var processCloudTelemetry = newCloudTelemetryShare()

// acquire returns key's shared runtime and resolver for database, building
// it under the share's own context and starting its poller when it is the
// first; release (idempotent) gives it back, and the last release stops
// the poller and waits for it. A new generation of the instance's only
// database (a reload) builds a fresh runtime, replacing the entry while
// the old generation still holds the old one; with other members the
// runtime is kept and polls at once.
func (s *cloudTelemetryShare) acquire(key, database string, build cloudBuild) (
	*cloudtel.Runtime, managedparam.Resolver, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[key]
	reload := e != nil && e.members[database] > 0
	var prev *cloudShareEntry
	if reload && len(e.members) == 1 {
		prev, e = e, nil // its only database reloads: rebuild it, as before sharing
	}
	if e == nil {
		ctx, cancel := context.WithCancel(context.Background())
		rt, resolver := build(ctx)
		if rt == nil {
			cancel()
			return nil, nil, func() {}
		}
		e = &cloudShareEntry{rt: rt, resolver: resolver, members: map[string]int{},
			cancel: cancel, done: make(chan struct{}), prev: prev}
		s.entries[key] = e
		go s.run(ctx, key, e)
	} else if reload {
		e.rt.PollNow() // other members hold it: poll at once instead
	}
	e.refs++
	e.members[database]++
	cloudtel.RegisterMember(database, e.rt)
	var once sync.Once
	return e.rt, e.resolver, func() { once.Do(func() { s.release(key, database, e) }) }
}

func (s *cloudTelemetryShare) release(key, database string, e *cloudShareEntry) {
	s.mu.Lock()
	e.refs--
	if e.members[database]--; e.members[database] <= 0 {
		delete(e.members, database)
		cloudtel.UnregisterMember(database, e.rt)
	}
	last := e.refs <= 0
	if last {
		s.retire(key, e)
	}
	s.mu.Unlock()
	if last {
		e.cancel()
		<-e.done
	}
}

// retire drops e, whose last holder released it (s.mu held). When e had
// replaced a runtime the database's running generation still holds (its
// reload was discarded), that runtime is the instance's entry again.
func (s *cloudTelemetryShare) retire(key string, e *cloudShareEntry) {
	if cur := s.entries[key]; cur != nil && cur.prev == e {
		cur.prev = nil // e is gone: nothing to restore
	}
	if s.entries[key] != e {
		return
	}
	delete(s.entries, key)
	if p := e.prev; p != nil && p.refs > 0 {
		s.entries[key] = p
		for db := range p.members {
			cloudtel.RegisterMember(db, p.rt)
		}
	}
}

// run polls until the last release; failures and recoveries are logged
// once per change, naming the instance's member databases.
func (s *cloudTelemetryShare) run(ctx context.Context, key string, e *cloudShareEntry) {
	defer close(e.done)
	e.rt.Run(ctx, func(err error) {
		logWarn("cloud", "%s: cloud telemetry: %v (retrying with backoff; reload the "+
			"config to retry now)", s.memberLabel(key, e), err)
	}, func() {
		logInfo("cloud", "%s: cloud telemetry recovered", s.memberLabel(key, e))
	})
}

func (s *cloudTelemetryShare) memberLabel(key string, e *cloudShareEntry) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(e.members))
	for db := range e.members {
		names = append(names, fmt.Sprintf("%q", db))
	}
	sort.Strings(names)
	return fmt.Sprintf("db %s (%s)", strings.Join(names, ", "), key)
}

// size is the number of shared runtimes (tests).
func (s *cloudTelemetryShare) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// cloudShareKey names the provider instance behind a database: its host
// and port when the host names the resource (an RDS endpoint, a Cloud SQL
// socket) or no resource is configured, else the configured resource, as
// cloudTelemetryFor resolves it. The polling settings are part of the key,
// so databases configured differently never share a poller.
func cloudShareKey(provider, host string, port uint16, r config.CloudResourceConfig,
	ct config.CloudTelemetryConfig) string {
	provider = managedparam.NormalizeProvider(provider)
	id := fmt.Sprintf("host=%s:%d", strings.ToLower(host), port)
	if named := configuredCloudResource(provider, host, r); named != "" {
		id = named
	}
	return fmt.Sprintf("%s|%s|every=%s|%+v", provider, id, ct.PollInterval(),
		cloudLimits(ct))
}

// configuredCloudResource is the configured resource that names the
// instance ("" when the host names it, or none is configured).
func configuredCloudResource(provider, host string, r config.CloudResourceConfig) string {
	switch provider {
	case "cloud-sql":
		if _, ok := cloudtel.ParseCloudSQLSocket(host); ok || r.GCP.Instance == "" {
			return ""
		}
		return strings.ToLower("gcp=" + r.GCP.Project + "/" + r.GCP.Instance)
	case "alloydb":
		if r.GCP.Cluster == "" {
			return ""
		}
		return strings.ToLower("alloydb=" + r.GCP.Project + "/" + r.GCP.Cluster + "/" +
			r.GCP.Instance)
	case "azure":
		if azure.ServerNameFromHost(host) != "" || r.Azure.Server == "" {
			return ""
		}
		return strings.ToLower("azure=" + r.Azure.Subscription + "/" + r.Azure.ResourceGroup +
			"/" + r.Azure.Server)
	}
	if _, ok := cloudtel.ParseRDSHost(host); ok {
		return ""
	}
	if r.AWS.DBInstanceIdentifier == "" && r.AWS.ClusterIdentifier == "" {
		return ""
	}
	return strings.ToLower("aws=" + r.AWS.Region + "/" + r.AWS.DBInstanceIdentifier + "/" +
		r.AWS.ClusterIdentifier)
}
