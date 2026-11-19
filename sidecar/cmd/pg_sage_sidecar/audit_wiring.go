package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auditjob"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/evidence"
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
	signer := checkpointSigner(evidenceSigner(cfg, control))
	auditSigner.Store(signer)
	noteCheckpointSigning(signer)
	sources := auditSources(control, mgr)
	startSIEMExport(ctx, control, sources)
	startChainVerification(ctx, sources)
}

// auditSources lists every database whose audit is chained: each fleet
// database, and the control database unless it is one of them. Each
// carries the running exporter's cursors, so verification sees a chain
// that went back behind its SIEM export.
func auditSources(control *pgxpool.Pool, mgr *fleet.DatabaseManager) func() []auditjob.Source {
	return func() []auditjob.Source {
		var out []auditjob.Source
		var cursors auditjob.CursorReader
		if e := auditExport.Load(); e != nil {
			cursors = e
		}
		full := fullVerifyHours()
		signer, prune := auditSigner.Load(), pruneBelowSignedCheckpoint()
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
					out = append(out, auditjob.Source{Name: name, Pool: inst.Pool,
						Cursors: cursors, FullVerifyHours: full, Signer: signer,
						PruneBelowSignedCheckpoint: prune})
				}
			}
		}
		if control != nil && !seen[control] {
			out = append(out, auditjob.Source{Name: "control", Pool: control,
				Cursors: cursors, FullVerifyHours: full, Signer: signer,
				PruneBelowSignedCheckpoint: prune})
		}
		return out
	}
}

// fullVerifyHours is audit.full_verify_interval_hours; without a config
// every pass is full (the safe, if slower, choice).
func fullVerifyHours() int {
	if cfg == nil {
		return 0
	}
	return cfg.Audit.FullVerifyIntervalHours
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
	for _, w := range siemStartupWarnings(sc.Sinks) {
		logWarn("audit", "%s", w)
	}
	e := siem.NewExporter(func() []siem.Source {
		var out []siem.Source
		for _, s := range sources() {
			out = append(out, siem.Source{Name: s.Name, DB: s.Pool})
		}
		return out
	}, sinks, siem.NewCursorStore(control), siem.Options{BatchSize: sc.BatchSize,
		Interval:        time.Duration(sc.IntervalSeconds) * time.Second,
		MaxBackoff:      time.Duration(sc.MaxBackoffSeconds) * time.Second,
		OnUndeliverable: siemUndeliverable(sources)},
		auditLeaderFence, func(f string, a ...any) { logWarn("audit", f, a...) })
	auditExport.Store(e)
	go e.Run(ctx)
	logInfo("audit", "SIEM export: %d sink(s), OCSF %s", len(sinks), siem.OCSFVersion)
}

// siemUndeliverable raises the siem_event_undeliverable finding on the
// database a stubbed event came from. A source no longer in the fleet
// fails the report, so its cursor holds rather than lose the record.
func siemUndeliverable(sources func() []auditjob.Source) func(context.Context,
	siem.Undeliverable) error {
	return func(ctx context.Context, u siem.Undeliverable) error {
		var pool *pgxpool.Pool
		for _, s := range sources() {
			if s.Name == u.Source {
				pool = s.Pool
				break
			}
		}
		return auditjob.RaiseUndeliverable(ctx, pool, u)
	}
}

// siemStartupWarnings is one line per unsafe sink, logged once at start:
// a token over plain http (refused from the next minor), and udp, which
// cannot confirm delivery.
func siemStartupWarnings(cfgs []config.AuditSIEMSink) []string {
	var out []string
	for _, s := range cfgs {
		for _, w := range s.Warnings() {
			if w == config.SIEMWarningTokenOverHTTP {
				out = append(out, fmt.Sprintf("SIEM sink %s sends its token over plain "+
					"http (%s); use https, or set allow_insecure_http: true. The next "+
					"minor release refuses this configuration", s.Name, w))
			}
		}
		if s.Type == "syslog" && s.Network == "udp" {
			out = append(out, fmt.Sprintf("SIEM sink %s: udp is best effort; lost "+
				"datagrams are not retried. Use tcp or tls for at-least-once delivery",
				s.Name))
		}
	}
	return out
}

// buildSIEMSinks builds the configured sinks; a token is read from its
// environment variable (or the variable's _FILE twin) and never logged.
// TLS files are read once, here; an error names the sink and the file.
func buildSIEMSinks(cfgs []config.AuditSIEMSink) ([]siem.SinkConfig, error) {
	out := make([]siem.SinkConfig, 0, len(cfgs))
	for _, s := range cfgs {
		sink, err := buildSIEMSink(s)
		if err != nil {
			return nil, err
		}
		out = append(out, siem.SinkConfig{Sink: sink, Chains: s.Chains,
			Warnings: s.Warnings()})
	}
	return out, nil
}

func buildSIEMSink(s config.AuditSIEMSink) (siem.Sink, error) {
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
	tlsCfg, err := siem.LoadTLSConfig(siem.TLSFiles{CA: s.CAFile, Cert: s.ClientCertFile,
		Key: s.ClientKeyFile})
	if err != nil {
		return nil, fmt.Errorf("sink %s: %w", s.Name, err)
	}
	switch s.Type {
	case "http":
		return siem.NewHTTPSink(s.Name, s.URL, token, timeout, tlsCfg), nil
	case "otlp":
		return siem.NewOTLPSink(s.Name, s.URL, token, timeout, tlsCfg), nil
	case "syslog":
		return siem.NewSyslogSink(s.Name, s.Network, s.Address, timeout, tlsCfg), nil
	}
	return nil, fmt.Errorf("sink %s: unknown type %q", s.Name, s.Type)
}

// auditLeaderFence lets the leader export, fenced by the lease it holds.
// The scope comes from the elector, never from cfg: a YAML reload changes
// cfg.Databases, and with it the scope fleetLearningScope would compute,
// but not the lease row the elector renews (fleet-02).
func auditLeaderFence() (siem.Fence, bool) {
	if !fleetLeaderAllows("SIEM export") {
		return siem.Fence{}, false
	}
	el := currentFleetLeader()
	if el == nil {
		return siem.Fence{}, true
	}
	tok, ok := el.Fence()
	if !ok {
		return siem.Fence{}, false
	}
	return siem.Fence{Scope: tok.Scope, Holder: tok.Holder, Epoch: tok.Epoch}, true
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
// Each run is recorded in auditjob.DefaultHealth for /metrics and the
// status route (audit-12): a follower drops what it recorded as leader,
// and a database no longer among the sources drops out.
func runChainVerification(ctx context.Context, sources []auditjob.Source) {
	health := auditjob.DefaultHealth()
	if !fleetLeaderAllows("audit chain verification") {
		health.Reset()
		return
	}
	names := make([]string, 0, len(sources))
	for _, src := range sources {
		names = append(names, src.Name)
	}
	health.Retain(names)
	for _, src := range sources {
		reports, err := auditjob.Run(ctx, src)
		health.Record(src.Name, reports, err, time.Now())
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

// siemStatusFunc reports the running exporter's sinks; nil when no sink
// is configured.
func siemStatusFunc(c *config.Config) func() []siem.SinkStatus {
	if c == nil || len(c.Audit.SIEM.Sinks) == 0 {
		return nil
	}
	return func() []siem.SinkStatus {
		if e := auditExport.Load(); e != nil {
			return e.Status()
		}
		return nil
	}
}

// errSIEMNotRunning answers a re-anchor while export is configured but
// did not start (see the startup log).
var errSIEMNotRunning = errors.New("SIEM export is configured but not running; " +
	"see the audit startup log")

// siemReanchorFunc re-anchors on the running exporter; nil when no sink is
// configured.
func siemReanchorFunc(c *config.Config) func(context.Context, siem.ReanchorRequest) (
	siem.ReanchorResult, error) {
	if c == nil || len(c.Audit.SIEM.Sinks) == 0 {
		return nil
	}
	return func(ctx context.Context, req siem.ReanchorRequest) (siem.ReanchorResult,
		error) {
		e := auditExport.Load()
		if e == nil {
			return siem.ReanchorResult{}, errSIEMNotRunning
		}
		return e.Reanchor(ctx, req)
	}
}

// evidenceSigningKey loads the Ed25519 key that signs evidence manifests;
// nil (hashed-only packs, logged once) when unset or unusable.
func evidenceSigningKey(c *config.Config) ed25519.PrivateKey {
	if c == nil || c.Audit.Evidence.SigningKeyEnv == "" {
		return nil
	}
	name := c.Audit.Evidence.SigningKeyEnv
	pemText, err := config.LookupSecretEnv(name)
	if err == nil && pemText == "" {
		err = fmt.Errorf("%s is empty", name)
	}
	if err != nil {
		logError("audit", "evidence packs will not be signed: %v", err)
		return nil
	}
	key, err := evidence.LoadSigningKey(pemText)
	if err != nil {
		logError("audit", "evidence packs will not be signed: %s: %v", name, err)
		return nil
	}
	return key
}
