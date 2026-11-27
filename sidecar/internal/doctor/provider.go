package doctor

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Platform is where the database runs: Code is the spec's platform word
// (SM, RDS, AUR, GCP, AZ, NEON, SUPA, LB) or "unknown", Provider the
// sidecar's provider name, Name what a person reads.
type Platform struct {
	Code     string `json:"code"`
	Provider string `json:"provider"`
	Name     string `json:"name"`
}

var unknownPlatform = Platform{Code: "unknown", Provider: "unknown",
	Name: "unrecognised platform"}

var (
	platformRDS      = Platform{"RDS", "rds", "Amazon RDS for PostgreSQL"}
	platformAurora   = Platform{"AUR", "aurora", "Amazon Aurora PostgreSQL"}
	platformCloudSQL = Platform{"GCP", "cloud-sql", "Google Cloud SQL for PostgreSQL"}
	platformAlloyDB  = Platform{"GCP", "alloydb", "Google AlloyDB"}
	platformAzure    = Platform{"AZ", "azure", "Azure Database for PostgreSQL"}
	platformNeon     = Platform{"NEON", "neon", "Neon"}
	platformSupabase = Platform{"SUPA", "supabase", "Supabase"}
	platformLakebase = Platform{"LB", "lakebase", "Databricks Lakebase"}
	platformSelf     = Platform{"SM", "self-managed", "self-managed PostgreSQL"}
)

// roleMarkers are the admin roles each managed platform creates, most
// specific first.
var roleMarkers = []struct {
	role     string
	platform Platform
}{
	{"alloydbsuperuser", platformAlloyDB},
	{"cloudsqlsuperuser", platformCloudSQL},
	{"azure_pg_admin", platformAzure},
	{"neon_superuser", platformNeon},
	{"supabase_admin", platformSupabase},
	{"databricks_superuser", platformLakebase},
	{"rds_superuser", platformRDS},
}

// ManagedRoles are the admin role names provider detection looks for.
func ManagedRoles() []string {
	out := make([]string, 0, len(roleMarkers))
	for _, m := range roleMarkers {
		out = append(out, m.role)
	}
	return out
}

// lakebaseHost matches Lakebase endpoints on AWS and Azure, like the
// sidecar's hosted-provider detection.
var lakebaseHost = regexp.MustCompile(
	`^[a-z0-9-]+\.database\.[a-z0-9-]+\.(cloud\.databricks\.com|azuredatabricks\.net)$`)

// ProviderFacts are what detection reads: the host name, the managed
// admin roles that exist, Aurora's aurora_version() and superuser.
type ProviderFacts struct {
	Host      string
	Roles     []string
	Aurora    bool
	Superuser bool
}

// DetectProvider names the platform from the facts. A managed admin role
// or host decides; a real superuser with neither is self-managed, since no
// managed service grants one. Anything else is unknown.
func DetectProvider(f ProviderFacts) Platform {
	if f.Aurora {
		return platformAurora
	}
	for _, m := range roleMarkers {
		if slices.Contains(f.Roles, m.role) {
			return m.platform
		}
	}
	if p, ok := platformFromHost(f.Host); ok {
		return p
	}
	if f.Superuser {
		return platformSelf
	}
	return unknownPlatform
}

// platformFromHost recognises the providers' official endpoint names. A
// name with URL or whitespace characters is not trusted.
func platformFromHost(host string) (Platform, bool) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" || strings.ContainsAny(host, "/:@\\ \t\r\n") {
		return Platform{}, false
	}
	switch {
	case lakebaseHost.MatchString(host):
		return platformLakebase, true
	case strings.HasSuffix(host, ".neon.tech"):
		return platformNeon, true
	case strings.HasSuffix(host, ".supabase.co"),
		strings.HasSuffix(host, ".pooler.supabase.com"):
		return platformSupabase, true
	case strings.HasSuffix(host, ".postgres.database.azure.com"),
		strings.HasSuffix(host, ".postgres.cosmos.azure.com"):
		return platformAzure, true
	case strings.HasSuffix(host, ".rds.amazonaws.com") && strings.Contains(host, ".cluster-"):
		return platformAurora, true
	case strings.HasSuffix(host, ".rds.amazonaws.com"):
		return platformRDS, true
	}
	return Platform{}, false
}

// providerFactsSQL reads, from the catalogs only, which managed admin
// roles exist, whether aurora_version() exists and whether the role is a
// superuser. to_regprocedure never raises for a missing function.
const providerFactsSQL = `/* pg_sage doctor */ SELECT r.rolsuper,
  ARRAY(SELECT g.rolname::text FROM pg_catalog.pg_roles g WHERE g.rolname = ANY($1)),
  pg_catalog.to_regprocedure('aurora_version()') IS NOT NULL
  FROM pg_catalog.pg_roles r WHERE r.rolname = current_user`

// readPlatform detects the target's platform; a failed read is unknown.
func readPlatform(ctx context.Context, pool *pgxpool.Pool) Platform {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	f := ProviderFacts{Host: pool.Config().ConnConfig.Host}
	if err := pool.QueryRow(qctx, providerFactsSQL, ManagedRoles()).Scan(&f.Superuser,
		&f.Roles, &f.Aurora); err != nil {
		return unknownPlatform
	}
	return DetectProvider(f)
}

// providerCheck passes for a recognised platform.
func providerCheck(p Platform) Check {
	c := Check{ID: IDProvider, Title: titleProvider}
	if p.Code == unknownPlatform.Code {
		c.Status = StatusUnknown
		c.Detail = "no managed admin role (" + strings.Join(ManagedRoles(), ", ") +
			") or provider host name found, and the role is not a superuser: " +
			"self-managed, or a platform pg_sage does not recognise"
		c.Fix = "set postgres.provider (fleet: databases[].provider), e.g. rds, cloud-sql, " +
			"neon or self-managed, when detection cannot tell"
		return c
	}
	c.Status = StatusPass
	c.Detail = p.Name + " (" + p.Code + ")"
	return c
}
