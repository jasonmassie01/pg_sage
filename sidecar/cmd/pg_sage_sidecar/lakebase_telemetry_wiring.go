package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/autoexplain"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/databricksauth"
	"github.com/pg-sage/sidecar/internal/lakebaseobs"
)

// History remains separate from rt.cloud: asynchronous gauges cannot admit a
// live maintenance action or supply current available RAM/free disk.
type lakebaseVerifiedBinding struct{ ScopeHash, PolicyBinding string }

type lakebaseTelemetryState struct {
	mu       sync.Mutex
	runtime  *lakebaseobs.Runtime
	store    *lakebaseobs.Store
	state    string
	payload  bool
	outcomes map[string]uint64
}

// A newer process can fence older pollers during overlapping App deployments.
var lakebaseHistoryProcessEpoch = time.Now().UTC()

var lakebaseTelemetryRegistry = struct {
	sync.Mutex
	entries map[string]*lakebaseTelemetryState
}{entries: map[string]*lakebaseTelemetryState{}}

func lakebaseTelemetryStatuses() any {
	lakebaseTelemetryRegistry.Lock()
	defer lakebaseTelemetryRegistry.Unlock()
	result := make(map[string]any, len(lakebaseTelemetryRegistry.entries))
	for name, st := range lakebaseTelemetryRegistry.entries {
		st.mu.Lock()
		counts := make(map[string]uint64, len(st.outcomes))
		for k, v := range st.outcomes {
			counts[k] = v
		}
		row := map[string]any{"state": st.state, "historical_only": true, "payload_enabled": st.payload, "complete": false, "exact_multiplicity": false, "delivery_outcomes_since_start": counts}
		if st.runtime != nil {
			row["streams"] = st.runtime.Status()
		}
		st.mu.Unlock()
		result[name] = row
	}
	return result
}
func (s *lakebaseTelemetryState) outcome(stream lakebaseobs.Stream, outcome string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outcomes[string(stream)+":"+outcome]++
}

func lakebaseTelemetryOptions(c config.LakebaseObservabilityConfig) lakebaseobs.Options {
	streams := make([]lakebaseobs.Stream, len(c.Streams))
	for i, s := range c.Streams {
		streams[i] = lakebaseobs.Stream(s)
	}
	return lakebaseobs.Options{WarehouseID: c.WarehouseID, AllowPayload: c.AllowPayload,
		MaxRows: c.MaxRows, MaxBytes: c.MaxBytes, QueriesPerPoll: c.QueriesPerPoll,
		PollInterval: c.PollInterval(), QueryTimeout: time.Duration(c.QueryTimeoutSeconds) * time.Second,
		Window: time.Duration(c.WindowSeconds) * time.Second, Retention: time.Duration(c.RetentionHours) * time.Hour,
		Streams: streams}
}
func (rt *databaseRuntime) startLakebaseTelemetry() {
	c := rt.cfg.LakebaseObservability
	if rt.provider != "lakebase" {
		return
	}
	rt.startLakebaseHistoryRetention(c)
	if !c.Enabled {
		return
	}
	conn := rt.spec.Pool.Config().ConnConfig
	binding, ok := c.Binding(conn.Host, conn.Database)
	if !ok {
		return
	}
	st := &lakebaseTelemetryState{state: "binding", payload: c.AllowPayload, outcomes: map[string]uint64{}}
	lakebaseTelemetryRegistry.Lock()
	lakebaseTelemetryRegistry.entries[rt.spec.Name] = st
	lakebaseTelemetryRegistry.Unlock()
	// Retry bounded metadata/identity failures, including a temporarily sleeping
	// endpoint. A binding is never inferred or broadened on failure.
	rt.start(func() {
		defer func() {
			lakebaseTelemetryRegistry.Lock()
			defer lakebaseTelemetryRegistry.Unlock()
			if lakebaseTelemetryRegistry.entries[rt.spec.Name] == st {
				delete(lakebaseTelemetryRegistry.entries, rt.spec.Name)
			}
		}()
		for {
			ctx, cancel := context.WithTimeout(rt.ctx, 45*time.Second)
			history, err := rt.bindLakebaseTelemetry(ctx, binding, st)
			cancel()
			if err == nil {
				st.mu.Lock()
				st.runtime = history
				st.state = "polling"
				st.mu.Unlock()
				rt.runLakebaseTelemetry(history, c)
				return
			}
			st.mu.Lock()
			st.state = "binding_unavailable"
			st.mu.Unlock()
			logWarn("lakebase_history", "db %q: binding unavailable; check explicit endpoint, SDK permissions and durable metadata", rt.spec.Name)
			select {
			case <-rt.ctx.Done():
				return
			case <-time.After(c.PollInterval()):
			}
		}
	})
	rt.note("lakebase_history")
}
func (rt *databaseRuntime) bindLakebaseTelemetry(ctx context.Context, b config.LakebaseObservabilityBinding, st *lakebaseTelemetryState) (*lakebaseobs.Runtime, error) {
	if rt.sre == nil {
		return nil, fmt.Errorf("registered SRE database identity unavailable")
	}
	registered, err := rt.sre.Bind(ctx)
	if err != nil {
		return nil, err
	}
	conn := rt.spec.Pool.Config().ConnConfig
	resource, err := databricksauth.ResolveTelemetryResource(ctx, conn.Host, b.Endpoint)
	if err != nil {
		return nil, err
	}
	var name string
	var oid int64
	if err = rt.spec.Pool.QueryRow(ctx, `/* pg_sage */ SELECT current_database(),oid::bigint FROM pg_database WHERE datname=current_database()`).Scan(&name, &oid); err != nil {
		return nil, err
	}
	if name != b.Database {
		return nil, lakebaseobs.ErrScope
	}
	scope := lakebaseobs.Scope{DeploymentID: string(registered.DeploymentID), DatabaseID: string(registered.DatabaseID), Workspace: resource.Workspace, ProjectID: resource.ProjectID, BranchID: resource.BranchID, EndpointID: resource.EndpointID, DatabaseName: name, DatabaseOID: oid, AllowCompute: b.AllowCompute, PolicyBinding: lakebasePolicyBinding(b)}
	client, err := databricksauth.StatementExecution()
	if err != nil {
		return nil, err
	}
	opts := lakebaseTelemetryOptions(rt.cfg.LakebaseObservability)
	reader, err := lakebaseobs.NewSDKReader(client, opts)
	if err != nil {
		return nil, err
	}
	store, err := lakebaseobs.NewStore(rt.spec.ControlPool)
	if err != nil {
		return nil, err
	}
	store = store.WithPolicyEpoch(lakebaseHistoryProcessEpoch)
	if err = store.EnsureSchema(ctx); err != nil {
		return nil, err
	}
	consumer := lakebaseobs.ConsumerFunc{ID: "normalized_history_v1", Handle: func(ctx context.Context, e lakebaseobs.Event) error {
		var err error
		switch e.Stream {
		case lakebaseobs.Plans:
			var p lakebaseobs.Plan
			p, err = lakebaseobs.NormalizePlan(e)
			if err == nil {
				err = autoexplain.StoreProviderPlan(ctx, rt.spec.Pool, autoexplain.ProviderPlan{
					ObservedPlan: autoexplain.ObservedPlan{QueryID: p.QueryID, Query: p.Query, JSON: p.JSON, TotalCost: p.TotalCost, ExecutionMS: p.ExecutionMS, CapturedAt: p.CapturedAt},
					ScopeHash:    e.ScopeHash, ComputeID: e.ComputeID, EventID: e.ID, ReceivedAt: e.ReceivedAt,
					AnalyzeEnabled: p.AnalyzeEnabled, BuffersEnabled: p.BuffersEnabled, TimingEnabled: p.TimingEnabled, Fingerprint: p.Fingerprint, DurationSource: p.DurationSource})
			}
		case lakebaseobs.Logs:
			_, err = lakebaseobs.NormalizeLog(e)
		case lakebaseobs.ASH:
			_, err = lakebaseobs.NormalizeASH(e)
		case lakebaseobs.DDL:
			_, err = lakebaseobs.NormalizeDDL(e)
		default:
			_, err = lakebaseobs.NormalizeMetrics(e)
		}
		// These rows remain durable historical evidence; unavailable payloads are
		// classified, never fabricated as healthy/no-error observations.
		outcome := "normalized"
		switch {
		case errors.Is(err, lakebaseobs.ErrMasked):
			outcome = "payload_unavailable"
		case errors.Is(err, lakebaseobs.ErrMalformed), errors.Is(err, lakebaseobs.ErrBudget):
			outcome = "rejected_row"
		case errors.Is(err, lakebaseobs.ErrSelfStatement), errors.Is(err, autoexplain.ErrSelfStatement):
			outcome = "self_excluded"
		case err != nil:
			return err
		}
		st.outcome(e.Stream, outcome)
		return nil
	}}
	if err = store.ActivateOwner(ctx, scope, rt.cfg.LakebaseObservability.AllowPayload); err != nil {
		return nil, err
	}
	st.mu.Lock()
	st.store = store
	st.mu.Unlock()
	runtime, err := lakebaseobs.NewRuntime(reader, store, scope, opts, consumer)
	if err == nil {
		rt.lakebaseScope.Store(lakebaseVerifiedBinding{ScopeHash: scope.Hash(), PolicyBinding: scope.PolicyBinding})
	}
	return runtime, err
}
func (rt *databaseRuntime) runLakebaseTelemetry(history *lakebaseobs.Runtime, c config.LakebaseObservabilityConfig) {
	tick := time.NewTicker(c.PollInterval())
	defer tick.Stop()
	for {
		err := history.Poll(rt.ctx, time.Now())
		if err != nil && rt.ctx.Err() == nil {
			logWarn("lakebase_history", "db %q: bounded history poll incomplete; inspect telemetry status", rt.spec.Name)
		}

		select {
		case <-rt.ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (rt *databaseRuntime) currentLakebaseScope() string {
	if rt.cfg == nil || rt.spec.Pool == nil || !rt.cfg.LakebaseObservability.Enabled || !rt.cfg.LakebaseObservability.AllowPayload {
		return ""
	}
	value, _ := rt.lakebaseScope.Load().(lakebaseVerifiedBinding)
	conn := rt.spec.Pool.Config().ConnConfig
	binding, ok := rt.cfg.LakebaseObservability.Binding(conn.Host, conn.Database)
	if !ok || value.PolicyBinding != lakebasePolicyBinding(binding) {
		return ""
	}
	return value.ScopeHash
}

func lakebasePolicyBinding(b config.LakebaseObservabilityBinding) string {
	raw := strings.ToLower(strings.TrimSuffix(b.Host, ".")) + "/" + b.Database + "/" + b.Endpoint
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Erasure has an independent lifecycle: disabling collection or losing SDK/UC
// access cannot suspend payload expiration. Ownership prevents cross-database
// deletion when the metadata/control pool is shared.
func (rt *databaseRuntime) startLakebaseHistoryRetention(c config.LakebaseObservabilityConfig) {
	rt.start(func() {
		store, err := lakebaseobs.NewStore(rt.spec.ControlPool)
		if err != nil {
			return
		}
		store = store.WithPolicyEpoch(lakebaseHistoryProcessEpoch)
		initialized := false
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			ctx, cancel := context.WithTimeout(rt.ctx, 20*time.Second)
			if !initialized {
				err = store.EnsureSchema(ctx)
				initialized = err == nil
			}
			if initialized {
				err = rt.cleanupLakebaseHistory(ctx, store, c, time.Now())
			}
			cancel()
			if err != nil && rt.ctx.Err() == nil {
				logWarn("lakebase_history", "db %q: history erasure pending; durable metadata unavailable", rt.spec.Name)
			}
			select {
			case <-rt.ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}
func (rt *databaseRuntime) cleanupLakebaseHistory(ctx context.Context, store *lakebaseobs.Store, c config.LakebaseObservabilityConfig, now time.Time) error {
	if err := store.Purge(ctx, now); err != nil {
		return err
	}
	if rt.sre == nil {
		return fmt.Errorf("history ownership unavailable")
	}
	registered, err := rt.sre.Bind(ctx)
	if err != nil {
		return err
	}
	dep, db := string(registered.DeploymentID), string(registered.DatabaseID)
	retention := time.Duration(c.RetentionHours) * time.Hour
	if retention == 0 {
		retention = lakebaseobs.SourceRetention
	}
	conn := rt.spec.Pool.Config().ConnConfig
	binding, bound := c.Binding(conn.Host, conn.Database)
	policy := ""
	if c.Enabled && bound {
		policy = lakebasePolicyBinding(binding)
	}
	verified, _ := rt.lakebaseScope.Load().(lakebaseVerifiedBinding)
	allowed := verified.ScopeHash
	if verified.PolicyBinding != policy {
		allowed = ""
	}
	if allowed != "" && policy != "" {
		err = store.CleanupOwner(ctx, dep, db, allowed, c.AllowPayload, retention, now)
	} else {
		err = store.CleanupOwnerPolicy(ctx, dep, db, policy, c.AllowPayload, retention, now)
	}
	if err != nil {
		return err
	}
	scopes, err := store.OwnerScopes(ctx, dep, db)
	if err != nil {
		return err
	}
	allowedScopes := []string{}
	if policy != "" && c.AllowPayload {
		if allowed != "" {
			allowedScopes = []string{allowed}
		} else {
			allowedScopes, err = store.PolicyOwnerScopes(ctx, dep, db, policy)
			if err != nil {
				return err
			}
		}
	}
	_, err = rt.spec.Pool.Exec(ctx, `/* pg_sage */ DELETE FROM sage.explain_cache
 WHERE source='lakebase_plan_history' AND provider_scope_hash=ANY($1::text[])
 AND (captured_at<$2 OR NOT (provider_scope_hash=ANY($3::text[])))`, scopes, now.Add(-retention), allowedScopes)
	return err
}

func lakebaseTelemetryHistory(ctx context.Context, database, stream string, from, to time.Time, limit int) (any, error) {
	lakebaseTelemetryRegistry.Lock()
	st := lakebaseTelemetryRegistry.entries[database]
	lakebaseTelemetryRegistry.Unlock()
	if st == nil {
		return nil, fmt.Errorf("history source unavailable")
	}
	st.mu.Lock()
	runtime, store := st.runtime, st.store
	st.mu.Unlock()
	if runtime == nil || store == nil {
		return nil, fmt.Errorf("history source unavailable")
	}
	events, err := store.History(ctx, runtime.Binding(), lakebaseobs.Stream(stream), from, to, limit, time.Now())
	if err != nil {
		return nil, err
	}
	rows := make([]map[string]any, 0, len(events))
	for _, e := range events {
		row := map[string]any{"event_id": e.ID, "stream": e.Stream, "scope_hash": e.ScopeHash, "compute_id": e.ComputeID, "occurred_at": e.OccurredAt, "received_at": e.ReceivedAt, "historical_only": true}
		var value any
		switch e.Stream {
		case lakebaseobs.Plans:
			value, err = lakebaseobs.NormalizePlan(e)
		case lakebaseobs.Logs:
			value, err = lakebaseobs.NormalizeLog(e)
		case lakebaseobs.ASH:
			value, err = lakebaseobs.NormalizeASH(e)
		case lakebaseobs.DDL:
			value, err = lakebaseobs.NormalizeDDL(e)
		default:
			value, err = lakebaseobs.NormalizeMetrics(e)
		}
		if err == nil {
			row["availability"] = "observed"
			row["observation"] = value
		} else if errors.Is(err, lakebaseobs.ErrMasked) {
			row["availability"] = "payload_unavailable"
		} else if errors.Is(err, lakebaseobs.ErrSelfStatement) {
			row["availability"] = "self_excluded"
		} else {
			row["availability"] = "rejected_row"
		}
		rows = append(rows, row)
	}
	return map[string]any{"historical_only": true, "complete": false, "exact_multiplicity": false, "from": from, "to": to, "rows": rows}, nil
}
