package main

import (
	"time"

	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/managedparam"
	"github.com/pg-sage/sidecar/internal/providerobs"
)

// supabaseStatus serves Supabase observability as a cloud telemetry
// status, so /api/v1/cloud-telemetry shows it. It is status only: the
// managed-cloud guards and Lookup never see it.
type supabaseStatus struct {
	database string // the fleet database name the API filters on
	resource string // Supabase project ref ("" when unknown)
	obs      *providerobs.Runtime
	now      func() time.Time
}

func newSupabaseStatus(database, resource string, obs *providerobs.Runtime) *supabaseStatus {
	return &supabaseStatus{database: database, resource: resource, obs: obs, now: time.Now}
}

func (s *supabaseStatus) Database() string { return s.database }

// Status maps fresh CPU and memory into a Sample; with neither fresh the
// status is unavailable with a reason.
func (s *supabaseStatus) Status() cloudtel.Status {
	st := cloudtel.Status{Database: s.database, Provider: "supabase", Resource: s.resource,
		Withhold: []string{}, Drift: []managedparam.Drift{}}
	now := s.now()
	t := s.obs.Telemetry()
	sample := cloudtel.Sample{Provider: "supabase", Resource: s.resource,
		CollectedAt: t.ObservedAt}
	if cpu, err := t.CPU(now); err == nil {
		sample.CPUPct = &cloudtel.Point{Value: cpu, At: t.ObservedAt}
	} else {
		sample.Missing = append(sample.Missing, "cpu_pct")
	}
	total, available := t.HostMemory(now)
	if total > 0 {
		sample.MemoryTotalBytes = &cloudtel.Point{Value: float64(total), At: t.ObservedAt}
		sample.MemoryTotalSource = cloudtel.MemorySourceSupabaseMetrics
	} else {
		sample.Missing = append(sample.Missing, "memory_total_bytes")
	}
	if available > 0 {
		sample.FreeableMemoryBytes = &cloudtel.Point{Value: float64(available),
			At: t.ObservedAt}
	} else {
		sample.Missing = append(sample.Missing, "freeable_memory_bytes")
	}
	if sample.CPUPct == nil && sample.MemoryTotalBytes == nil {
		st.Reason = "unavailable: no Supabase metrics from the last " +
			providerobs.MaxMetricAge.String() + "; check the observability token's " +
			"access and the sidecar log"
		return st
	}
	observed := t.ObservedAt
	st.Available, st.Sample = true, &sample
	st.LastSuccessAt = &observed
	return st
}

// attachProviderObservability gives the memory gates Supabase memory and
// publishes its status for this database until the runtime stops. In
// fleet mode each database has its own runtime and status entry.
func (rt *databaseRuntime) attachProviderObservability(obs *providerobs.Runtime) {
	if obs == nil {
		return
	}
	rt.providerObs.Store(obs)
	if rt.cloud != nil {
		return // managed-cloud telemetry owns this database's status entry
	}
	ref := ""
	if rt.spec.Pool != nil {
		conn := rt.spec.Pool.Config().ConnConfig
		ref = supabaseObservabilityProject(conn.Host, conn.User)
	}
	status := newSupabaseStatus(rt.spec.Name, ref, obs)
	cloudtel.Register(status)
	rt.start(func() {
		<-rt.ctx.Done()
		cloudtel.Unregister(status)
	})
}
