package main

import (
	"context"
	"fmt"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/rds"

	"github.com/pg-sage/sidecar/internal/databricksauth"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/fleet/discovery"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// fleetDiscoveryInterval is the period between discovery cycles; a policy
// set's enrollment.interval_minutes may shorten it (to 2 minutes at least).
const fleetDiscoveryInterval = 10 * time.Minute

// startFleetDiscovery starts fleet auto-discovery (market-06) in a meta-db
// fleet when fleet.discovery.enabled. A source that cannot be built stops
// discovery as a whole: it fails closed and says why.
func startFleetDiscovery(state *metaDBState) {
	dc := cfg.Fleet.Discovery
	if !dc.Enabled {
		return
	}
	bindings, err := discovery.Bindings(dc, metaDiscoverySourceDeps(state))
	if err != nil {
		logWarn("discovery", "fleet discovery disabled: %v", err)
		return
	}
	members := metaDiscoveryMembers{mgr: fleetMgr, state: state}
	runner, err := discovery.NewRunner(dc, bindings,
		store.NewDiscoveryStore(state.Pool), members)
	if err != nil {
		logWarn("discovery", "fleet discovery disabled: %v", err)
		return
	}
	// fleetpol-01b: enroll by policy set, and ask each member's first grade.
	runner.WithPolicySets(discovery.PolicySetsFrom(policy.NewStore(state.Pool)),
		newMetaDiscoveryGrades())
	fleetDiscoveryRunner.Store(runner)
	logInfo("discovery", "fleet discovery: %d source(s) by policy set, max_enrolled %d, "+
		"ttl %dh", len(bindings), dc.MaxEnrolled, dc.TTLHours)
	go runner.Run(shutdownCtx, fleetDiscoveryInterval)
}

// warnDiscoveryOutsideMetaDB says that fleet.discovery is ignored: it
// enrols into sage.databases, which only a meta-db fleet has.
func warnDiscoveryOutsideMetaDB() {
	if cfg != nil && cfg.Fleet.Discovery.Enabled && !cfg.HasMetaDB() {
		logWarn("discovery", "fleet.discovery.enabled is ignored: discovery needs a "+
			"meta-db fleet (--meta-db)")
	}
}

func metaDiscoverySourceDeps(state *metaDBState) discovery.SourceDeps {
	return discovery.SourceDeps{
		ClusterPool: func(member string) (discovery.Beginner, error) {
			inst := fleetMgr.GetInstance(member)
			if inst == nil || inst.Pool == nil {
				return nil, fmt.Errorf("member %q is not connected", member)
			}
			return inst.Pool, nil
		},
		Member: func(member string) (string, int, error) {
			rec, err := storeRecordByName(context.Background(), state.Store, member)
			if err != nil {
				return "", 0, err
			}
			return rec.Host, rec.Port, nil
		},
		Lakebase: func() (discovery.LakebaseAPI, error) {
			return databricksauth.PostgresAPI()
		},
		RDS: func(region string) (discovery.RDSAPI, error) {
			ctx, cancel := context.WithTimeout(context.Background(), metaDBTimeout)
			defer cancel()
			awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
			if err != nil {
				return nil, fmt.Errorf("loading AWS configuration for %s: %w", region, err)
			}
			return rds.NewFromConfig(awsCfg), nil
		},
		Neon: neonDiscoveryAPI,
	}
}

func storeRecordByName(ctx context.Context, s *store.DatabaseStore,
	name string) (store.DatabaseRecord, error) {
	records, err := s.List(ctx)
	if err != nil {
		return store.DatabaseRecord{}, err
	}
	for _, r := range records {
		if r.Name == name {
			return r, nil
		}
	}
	return store.DatabaseRecord{}, fmt.Errorf("no managed database named %q", name)
}

// metaDiscoveryMembers enrols discovered members through the same
// lifecycle as an API create, and disables them through the delete path.
type metaDiscoveryMembers struct {
	mgr   *fleet.DatabaseManager
	state *metaDBState
}

func (m metaDiscoveryMembers) Enrol(ctx context.Context, from string,
	row store.DiscoveredRow, expiresAt time.Time) (int, error) {
	fromRec, err := storeRecordByName(ctx, m.state.Store, from)
	if err != nil {
		return 0, fmt.Errorf("credential member: %w", err)
	}
	name := discovery.MemberName(row)
	member := store.DiscoveredMember{Name: name, DatabaseName: row.DBName,
		Source: row.Source, Owner: row.Owner, ExpiresAt: expiresAt, Tags: row.Tags,
		Tier: row.Tier}
	if row.Tier == store.TierObserveOnly {
		// fleetpol-01d: no runtime; the observe-only sweeper visits it.
		id, err := m.state.Store.CreateDiscovered(ctx, fromRec.ID, member)
		if err == nil {
			logInfo("discovery", "db %q: enrolled observe-only from %s until %s", name,
				row.Source, expiresAt.Format(time.RFC3339))
		}
		return id, err
	}
	rec, err := applyMetaDatabaseCreateWith(ctx, m.mgr, m.state, name,
		func(ctx context.Context) (int, error) {
			return m.state.Store.CreateDiscovered(ctx, fromRec.ID, member)
		})
	if err != nil {
		return 0, err
	}
	logInfo("discovery", "db %q: enrolled from %s at observation until %s", name,
		row.Source, expiresAt.Format(time.RFC3339))
	return rec.ID, nil
}

func (m metaDiscoveryMembers) Disable(ctx context.Context, databaseID int) error {
	retired, err := deleteManagedDatabase(ctx, m.mgr, databaseID,
		func(ctx context.Context) error {
			return m.state.Store.DisableDiscovered(ctx, databaseID)
		})
	if err != nil {
		return err
	}
	if err := fleet.ShutdownInstance(context.WithoutCancel(ctx), retired); err != nil {
		logWarn("discovery", "db %d: disabled; runtime drain incomplete: %v",
			databaseID, err)
	}
	if retired != nil && m.mgr.GetInstance(retired.Name) == nil {
		forgetRemovedDatabase(retired.Name)
	}
	return nil
}
