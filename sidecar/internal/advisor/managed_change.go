package advisor

import (
	"maps"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/managedparam"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

// withManagedIntent records, on a finding whose setting SQL cannot change
// on RDS, Aurora, Cloud SQL, AlloyDB or Azure flexible server, the typed
// managed change it needs (a parameter group, database flag or server
// parameter). The managed-change worker turns it
// into a proposal an operator approves and applies; pg_sage never
// applies it. Other platforms and unparseable SQL are left unchanged.
func withManagedIntent(f analyzer.Finding, platform, sql, why string) analyzer.Finding {
	if !managedparam.Supported(platform) {
		return f
	}
	stmt, ok := pgconf.ParseAlterSystem(sql)
	if !ok || stmt.Reset || stmt.Value == "" {
		return f
	}
	in, err := managedparam.NewIntent(platform, stmt.Name, pgconf.Unquote(stmt.Value), why)
	if err != nil {
		return f
	}
	f.Detail = maps.Clone(f.Detail)
	if f.Detail == nil {
		f.Detail = map[string]any{}
	}
	f.Detail[managedparam.DetailKey] = in.Detail()
	f.Detail[analyzer.DetailApprovalRequired] = "a " + platform + " " +
		mechanismLabel(platform) + " change: an operator applies it"
	return f
}

func mechanismLabel(platform string) string {
	switch managedparam.NormalizeProvider(platform) {
	case "cloud-sql", "alloydb":
		return "database flag"
	case "azure":
		return "server parameter"
	case "cnpg":
		return "Cluster spec parameter"
	}
	return "parameter group"
}
