package advisor

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/collector"
)

// promptEnv is what every sub-advisor prompt states about the database it
// reviews: the hosting platform and the PostgreSQL major version (0 when
// unknown). Analyze resolves it once per review (advisors-06).
type promptEnv struct {
	platform string
	pgMajor  int
}

type promptEnvKey struct{}

// WithPGVersion sets the server_version_num (e.g. 170004) of the reviewed
// database, so prompts carry the version notes that hold on its major.
func (a *Advisor) WithPGVersion(versionNum int) {
	a.pgMajor = versionNum / 10000
}

// platform is the reviewed database's provider: the per-instance override,
// else the configured cloud environment ("" when neither is known).
func (a *Advisor) platform() string {
	if a.cloudEnv != "" {
		return a.cloudEnv
	}
	return a.cfg.CloudEnvironment
}

// database is the PostgreSQL database name the platform rewrites target.
func (a *Advisor) database() string {
	if a.dbName != "" {
		return a.dbName
	}
	return a.cfg.Postgres.Database
}

// reviewContext carries this review's confirmed facts and prompt
// environment to every sub-advisor.
func (a *Advisor) reviewContext(ctx context.Context) context.Context {
	env := promptEnv{platform: a.platform(), pgMajor: a.pgMajor}
	return context.WithValue(a.factsContext(ctx), promptEnvKey{}, env)
}

// review runs the sub-advisors on one snapshot pair with this review's
// prompt environment, then gates their findings for the same platform
// and database.
func (a *Advisor) review(ctx context.Context, snap *collector.Snapshot) subRun {
	cloudEnv, dbName := a.platform(), a.database()
	r := a.runSubAdvisors(a.reviewContext(ctx), snap)
	r.findings = GateConfigFindingsHost(r.findings, a.hostMemory(), cloudEnv, dbName,
		snap.ConfigData.PGSettings)
	own := ownershipFrom(ctx)
	r.findings = GateInstanceSettings(r.findings, own.owner, own.isOwner)
	return r
}

// promptEnvFrom is the review's environment; without one the platform is
// unknown to the prompt and reads as self-managed, version unknown.
func promptEnvFrom(ctx context.Context) promptEnv {
	env, _ := ctx.Value(promptEnvKey{}).(promptEnv)
	return env
}

// restrictedSettings are the settings each provider's prompt tells the
// model never to recommend. Only the prompt reads it: whether a proposal
// can run as ALTER DATABASE is decided by the setting's pg_settings
// context (TransformForCloud), which also covers providers listed here.
var restrictedSettings = map[string]map[string]bool{
	"cloud-sql": {
		"wal_level": true, "full_page_writes": true,
		"shared_buffers": true, "checkpoint_timeout": true,
	},
	"alloydb": {
		"wal_level": true, "full_page_writes": true,
		"shared_buffers": true, "checkpoint_timeout": true,
	},
	"aurora": {
		"wal_level": true, "full_page_writes": true,
		"max_wal_size": true, "min_wal_size": true,
		"checkpoint_timeout": true,
	},
	"rds": {
		"wal_level": true, "full_page_writes": true,
		"max_wal_size": true, "min_wal_size": true,
		"checkpoint_timeout": true,
	},
}

// platformSettingsSurface is where each managed provider changes
// instance-level parameters.
var platformSettingsSurface = map[string]string{
	"rds":       "the DB parameter group",
	"aurora":    "the DB cluster or instance parameter group",
	"cloud-sql": "Cloud SQL database flags",
	"alloydb":   "AlloyDB database flags",
	"azure":     "Azure server parameters (portal or CLI)",
	"neon":      "Neon's project settings or support (availability is not guaranteed)",
	"supabase":  "Supabase's config API or CLI",
	"lakebase":  "Lakebase, which manages instance settings itself",
	"cnpg": "the CloudNativePG Cluster's spec.postgresql.parameters (kubectl patch " +
		"cluster or the GitOps repository)",
}

// platformNote is the trusted platform statement for a sub-advisor's
// system prompt. A managed provider is told ALTER SYSTEM is unavailable,
// where its parameters change and which settings it never adjusts; the
// note never offers ALTER DATABASE for server settings (advisors-01).
// GateConfigFindingsHost still enforces all of this on the answer.
func platformNote(platform string) string {
	p := strings.ToLower(strings.TrimSpace(platform))
	if p == "unknown" {
		return "Platform: unknown (the provider probe has not identified the host). " +
			"Do not assume ALTER SYSTEM can be used; say in the rationale that the " +
			"change may have to go through the provider's console."
	}
	if !IsManagedService(p) {
		return "Platform: self-managed (ALTER SYSTEM is available; restart-only " +
			"settings need an operator-scheduled restart)."
	}
	note := fmt.Sprintf("Platform: %s (managed service). ALTER SYSTEM unavailable: "+
		"instance parameters change through %s, not through ALTER DATABASE. "+
		"Keep recommended_sql in the ALTER SYSTEM SET form; pg_sage adapts or "+
		"withholds it for the platform.", p, platformSettingsSurface[p])
	if fixed := restrictedNames(p); fixed != "" {
		note += " Not adjustable on " + p + " (never recommend them): " + fixed + "."
	}
	return note
}

// restrictedNames lists the platform's non-adjustable settings, sorted.
func restrictedNames(platform string) string {
	names := make([]string, 0, len(restrictedSettings[platform]))
	for name := range restrictedSettings[platform] {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// promptGrounding is the trusted, code-authored part of a GUC sub-advisor's
// system prompt: the platform note and the documentation prior of the
// GUCs it tunes on the reviewed major version.
func promptGrounding(ctx context.Context, gucs ...string) string {
	env := promptEnvFrom(ctx)
	grounding := platformNote(env.platform)
	if docs := DocContext(env.pgMajor, gucs...); docs != "" {
		grounding += "\n\n" + docs
	}
	return grounding
}
