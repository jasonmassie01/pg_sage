package main

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/azure"
	"github.com/pg-sage/sidecar/internal/config"
	prov "github.com/pg-sage/sidecar/internal/provider"
	"github.com/pg-sage/sidecar/internal/provideradvice"
)

// providerAdviceInterval is how often provider advice is re-read: the
// providers refresh their advisors daily or slower.
const providerAdviceInterval = time.Hour

// postgresDatabaseName is the PostgreSQL database a connection opens: its
// dbname, or the user name when none is given (the server's default).
// Index tuning names databases this way, not by pg_sage's instance name.
func postgresDatabaseName(dbname, user string) string {
	if dbname != "" {
		return dbname
	}
	return user
}

// adviceTarget is the database whose provider advice is read; database is
// the PostgreSQL database name.
type adviceTarget struct {
	provider, host, user, database string
	pool                           *pgxpool.Pool
	azure                          config.CloudResourceAzure
}

// providerAdviceSources are the database's advice sources and, when one
// cannot run, why: the advisor view (Cloud SQL, AlloyDB), Azure index
// tuning (the ARM identity host telemetry uses) and Supabase advisors
// (the observability token, the project the endpoint names). Neon has no
// advisor API. Nothing here makes a network call.
func providerAdviceSources(c *config.Config, t adviceTarget, deps cloudDeps,
	supabaseToken string) ([]provideradvice.Source, string) {
	if c == nil || !c.ProviderAdvice.IsEnabled() || t.pool == nil {
		return nil, ""
	}
	switch name := prov.Canonical(t.provider); name {
	case prov.CloudSQL, prov.AlloyDB:
		src, err := provideradvice.NewCloudSQLSource(name, t.pool)
		return adviceSource(src, err)
	case prov.Azure:
		src, err := azureAdviceSource(c, t, deps)
		return adviceSource(src, err)
	case prov.Neon:
		return nil, "neon has no advisor API; pg_sage's own index findings stand alone"
	}
	ref := supabaseObservabilityProject(t.host, t.user)
	if ref == "" {
		return nil, ""
	}
	if supabaseToken == "" {
		return nil, "supabase advisors need SAGE_SUPABASE_OBSERVABILITY_TOKEN"
	}
	src, err := provideradvice.NewSupabaseSource(provideradvice.SupabaseOptions{
		Project: ref, Token: supabaseToken})
	return adviceSource(src, err)
}

func adviceSource[S provideradvice.Source](src S, err error) ([]provideradvice.Source,
	string) {
	if err != nil {
		return nil, err.Error()
	}
	return []provideradvice.Source{src}, ""
}

// azureAdviceSource names the flexible server as host telemetry does: the
// host's server name, else cloud_resource.azure or azure.*.
func azureAdviceSource(c *config.Config, t adviceTarget,
	deps cloudDeps) (*provideradvice.AzureSource, error) {
	target := azureTargetConfig(c, t.azure).Azure
	server := azure.ServerNameFromHost(t.host)
	if server == "" {
		server = target.ServerName
	}
	switch {
	case target.SubscriptionID == "" || target.ResourceGroup == "":
		return nil, errors.New("azure index tuning needs the server's resource ID; set " +
			"azure.subscription_id and azure.resource_group (or cloud_resource.azure)")
	case server == "":
		return nil, errors.New("host " + t.host + " does not name the flexible server; " +
			"set azure.server_name (or cloud_resource.azure.server)")
	}
	token, err := deps.azureToken()
	if err != nil {
		return nil, err
	}
	return provideradvice.NewAzureSource(provideradvice.AzureOptions{
		SubscriptionID: target.SubscriptionID, ResourceGroup: target.ResourceGroup,
		Server: server, Database: t.database, Token: token})
}

// adviceTarget is the runtime's database as its provider names it: the
// pool's host, user and PostgreSQL database (not the fleet instance name).
func (rt *databaseRuntime) adviceTarget() adviceTarget {
	cc := rt.spec.Pool.Config().ConnConfig
	return adviceTarget{provider: rt.provider, host: cc.Host, user: cc.User,
		database: postgresDatabaseName(cc.Database, cc.User), pool: rt.spec.Pool,
		azure: rt.spec.Config.CloudResource.Azure}
}

// startProviderAdvice reconciles the provider's advice hourly. It is
// evidence only: nothing it writes reaches the executor or the policy
// gate.
func (rt *databaseRuntime) startProviderAdvice() {
	token, err := config.LookupSecretEnv("SAGE_SUPABASE_OBSERVABILITY_TOKEN")
	if err != nil {
		logWarn(rt.spec.Scope, "db %q: supabase advisors unavailable: %v", rt.spec.Name, err)
	}
	sources, note := providerAdviceSources(rt.cfg, rt.adviceTarget(), defaultCloudDeps(),
		token)
	if note != "" {
		logInfo(rt.spec.Scope, "db %q: provider advice: %s", rt.spec.Name, note)
	}
	if len(sources) == 0 {
		return
	}
	pool := rt.spec.Pool
	runner, err := provideradvice.NewRunner(provideradvice.RunnerOptions{
		Database: rt.spec.Name, Sources: sources, Sink: provideradvice.NewStore(pool),
		Findings: func(ctx context.Context) ([]provideradvice.Finding, error) {
			return provideradvice.LoadFindings(ctx, pool)
		},
		Eval: provideradvice.NewDBEvaluator(pool, logStructuredWrapper)})
	if err != nil {
		logWarn(rt.spec.Scope, "db %q: provider advice unavailable: %v", rt.spec.Name, err)
		return
	}
	rt.start(func() {
		runner.Run(rt.ctx, providerAdviceInterval, func(err error) {
			logWarn(rt.spec.Scope, "db %q: provider advice: %v", rt.spec.Name, err)
		})
	})
	rt.note("provider_advice")
}
