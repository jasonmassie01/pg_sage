package main

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auditjob"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/siem"
)

// auditExport is the running SIEM exporter (nil without sinks); the API
// reports its sinks' health.
var auditExport atomic.Pointer[siem.Exporter]

// startAuditJobs starts the E2 singleton jobs: SIEM export and scheduled
// verification of the audit chains. Both run on the fleet leader only.
func startAuditJobs(ctx context.Context, control *pgxpool.Pool,
	mgr *fleet.DatabaseManager) {
	if cfg == nil {
		return
	}
	sources := auditSources(control, mgr)
	startSIEMExport(ctx, control, sources)
	startChainVerification(ctx, sources)
}

// auditSources lists every database whose audit is chained: each fleet
// database, and the control database unless it is one of them.
func auditSources(control *pgxpool.Pool, mgr *fleet.DatabaseManager) func() []auditjob.Source {
	return func() []auditjob.Source {
		var out []auditjob.Source
		seen := map[*pgxpool.Pool]bool{}
		if mgr != nil {
			instances := mgr.Instances()
			names := make([]string, 0, len(instances))
			for name := range instances {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if inst := instances[name]; inst != nil && inst.Pool != nil && !seen[inst.Pool] {
					seen[inst.Pool] = true
					out = append(out, auditjob.Source{Name: name, Pool: inst.Pool})
				}
			}
		}
		if control != nil && !seen[control] {
			out = append(out, auditjob.Source{Name: "control", Pool: control})
		}
		return out
	}
}

func startSIEMExport(ctx context.Context, control *pgxpool.Pool,
	sources func() []auditjob.Source) {
	sc := cfg.Audit.SIEM
	if len(sc.Sinks) == 0 || control == nil {
		return
	}
	sinks, err := buildSIEMSinks(sc.Sinks)
	if err != nil {
		logError("audit", "SIEM export is off: %v", err)
		return
	}
	e := siem.NewExporter(func() []siem.Source {
		var out []siem.Source
		for _, s := range sources() {
			out = append(out, siem.Source{Name: s.Name, DB: s.Pool})
		}
		return out
	}, sinks, siem.NewCursorStore(control), siem.Options{BatchSize: sc.BatchSize,
		Interval:   time.Duration(sc.IntervalSeconds) * time.Second,
		MaxBackoff: time.Duration(sc.MaxBackoffSeconds) * time.Second},
		auditLeaderFence, func(f string, a ...any) { logWarn("audit", f, a...) })
	auditExport.Store(e)
	go e.Run(ctx)
	logInfo("audit", "SIEM export: %d sink(s), OCSF %s", len(sinks), siem.OCSFVersion)
}

// buildSIEMSinks builds the configured sinks; a token is read from its
// environment variable (or the variable's _FILE twin) and never logged.
func buildSIEMSinks(cfgs []config.AuditSIEMSink) ([]siem.SinkConfig, error) {
	out := make([]siem.SinkConfig, 0, len(cfgs))
	for _, s := range cfgs {
		token := ""
		if s.TokenEnv != "" {
			v, err := config.LookupSecretEnv(s.TokenEnv)
			if err != nil || v == "" {
				return nil, fmt.Errorf("sink %s: token variable %s is unset or unreadable",
					s.Name, s.TokenEnv)
			}
			token = v
		}
		timeout := time.Duration(s.TimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = config.DefaultSIEMTimeoutSeconds * time.Second
		}
		var sink siem.Sink
		switch s.Type {
		case "http":
			sink = siem.NewHTTPSink(s.Name, s.URL, token, timeout)
		case "otlp":
			sink = siem.NewOTLPSink(s.Name, s.URL, token, timeout)
		case "syslog":
			sink = siem.NewSyslogSink(s.Name, s.Network, s.Address, timeout, nil)
		default:
			return nil, fmt.Errorf("sink %s: unknown type %q", s.Name, s.Type)
		}
		out = append(out, siem.SinkConfig{Sink: sink, Chains: s.Chains})
	}
	return out, nil
}

// auditLeaderFence lets the leader export, fenced by its lease epoch.
func auditLeaderFence() (siem.Fence, bool) {
	if !fleetLeaderAllows("SIEM export") {
		return siem.Fence{}, false
	}
	if fleetLeader == nil {
		return siem.Fence{}, true
	}
	holder, epoch, ok := fleetLeader.Fence()
	if !ok {
		return siem.Fence{}, false
	}
	return siem.Fence{Scope: fleetLearningScope(cfg), Holder: holder, Epoch: epoch}, true
}

func startChainVerification(ctx context.Context, sources func() []auditjob.Source) {
	hours := cfg.Audit.VerifyIntervalHours
	if hours <= 0 {
		return
	}
	go afterDelay(ctx, firstCycleDelay, func() {
		every(ctx, time.Duration(hours)*time.Hour, func(c context.Context) {
			runChainVerification(c, sources())
		})
	})
}

// runChainVerification verifies every source's chains on the leader; a
// failing trail raises a critical finding (auditjob) and an error line.
func runChainVerification(ctx context.Context, sources []auditjob.Source) {
	if len(sources) == 0 || !fleetLeaderAllows("audit chain verification") {
		return
	}
	for _, src := range sources {
		reports, err := auditjob.Run(ctx, src)
		if err != nil {
			logWarn("audit", "audit chain verification of %s failed: %v", src.Name, err)
			continue
		}
		for _, rep := range reports {
			if !rep.OK() {
				logError("audit", "audit trail %s of %s fails verification: %d problem(s)",
					rep.Chain, src.Name, len(rep.Problems))
			}
		}
	}
}
