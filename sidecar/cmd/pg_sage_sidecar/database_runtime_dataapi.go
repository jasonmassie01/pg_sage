package main

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/agentposture"
	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
	prov "github.com/pg-sage/sidecar/internal/provider"
)

// Supabase Data API exposure (fleetpol-02c): on a Supabase database whose
// endpoint names its project, cloud telemetry reads which schemas the
// project's Data API serves (one Management API call an hour, with the
// observability token) and posture's AP-03 and AP-04 use it. Read only;
// without a reading every finding keeps its severity (unknown).

// supabaseDataAPIReader is the Data API exposure reader for the database,
// and a note when a Supabase database cannot have one. Off Supabase there
// is neither. Nothing here makes a network call.
func supabaseDataAPIReader(provider, host, user,
	token string) (*cloudtel.SupabaseDataAPI, string) {
	if prov.Canonical(provider) != prov.Supabase {
		return nil, ""
	}
	ref := supabaseObservabilityProject(host, user)
	if ref == "" {
		return nil, "host " + host + " does not name the project, so the Data API " +
			"exposure stays unknown"
	}
	if strings.TrimSpace(token) == "" {
		return nil, "the Data API exposure needs SAGE_SUPABASE_OBSERVABILITY_TOKEN; it " +
			"stays unknown"
	}
	api, err := cloudtel.NewSupabaseHTTPAPI(cloudtel.SupabaseHTTPOptions{Project: ref,
		Token: token})
	if err != nil {
		return nil, err.Error()
	}
	return cloudtel.NewSupabaseDataAPI(api, cloudtel.SupabaseDataAPIOptions{
		Logf: func(format string, args ...any) {
			logWarn("provider", "supabase project "+ref+": "+format, args...)
		}}), ""
}

// startSupabaseDataAPI starts the reader before any posture run reads it.
func (rt *databaseRuntime) startSupabaseDataAPI() {
	if prov.Canonical(rt.provider) != prov.Supabase {
		return
	}
	token, err := config.LookupSecretEnv("SAGE_SUPABASE_OBSERVABILITY_TOKEN")
	if err != nil {
		logWarn(rt.spec.Scope, "db %q: supabase data API exposure unavailable: %v",
			rt.spec.Name, err)
	}
	t := rt.adviceTarget()
	reader, note := supabaseDataAPIReader(rt.provider, t.host, t.user, token)
	if note != "" {
		logInfo(rt.spec.Scope, "db %q: %s", rt.spec.Name, note)
	}
	if reader == nil {
		return
	}
	rt.dataAPI = reader
	rt.start(func() { reader.Run(rt.ctx) })
	rt.note("supabase_data_api")
}

// postureDataAPI is the reader's last exposure for posture; nil (unknown)
// without a reader or a successful read.
func postureDataAPI(r *cloudtel.SupabaseDataAPI) *agentposture.DataAPIExposure {
	e := r.Exposure()
	if e == nil {
		return nil
	}
	return &agentposture.DataAPIExposure{Schemas: e.Schemas}
}
