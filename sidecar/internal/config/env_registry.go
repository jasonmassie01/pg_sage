package config

import "fmt"

// EnvVar documents one SAGE_* environment variable pg_sage reads
// (config-08). cmd/gen_config_meta renders the runtime entries into
// docs/configuration.md and the contributor entries into CONTRIBUTING.md,
// and a drift test fails when a "SAGE_..." literal in a non-test file under
// internal/, cmd/ or the bench harnesses is not registered here (ci-10).
type EnvVar struct {
	Name string
	// Default is what applies when the variable is unset, as Markdown.
	Default string
	// Doc is one sentence for the docs table.
	Doc string
	// File is true when NAME_FILE may hold the value instead.
	File bool
	// Kind says who sets the variable.
	Kind EnvKind
}

// EnvKind says who sets a variable and so which table documents it.
type EnvKind string

const (
	// EnvRuntime variables configure a running sidecar or one of its
	// subcommands; docs/configuration.md lists them.
	EnvRuntime EnvKind = "runtime"
	// EnvContributor variables steer the test suites and benches;
	// CONTRIBUTING.md lists them.
	EnvContributor EnvKind = "contributor"
)

const (
	envNone  = "(none)"
	envUnset = "(unset)"
)

func envCode(v any) string {
	return fmt.Sprintf("`%v`", v)
}

// envRegistry is every entry, runtime first, in documentation order.
var envRegistry = append(withKind(EnvRuntime, runtimeEnvVars),
	withKind(EnvContributor, contributorEnvVars)...)

// withKind returns a copy of vars with Kind set to kind.
func withKind(kind EnvKind, vars []EnvVar) []EnvVar {
	out := make([]EnvVar, len(vars))
	for i, v := range vars {
		v.Kind = kind
		out[i] = v
	}
	return out
}

var runtimeEnvVars = []EnvVar{
	{Name: "SAGE_CONFIG_PATH", Default: envNone, Doc: "Path to the YAML config file when " +
		"`--config` is not given; otherwise `config.yaml` in the working directory is used " +
		"if present"},
	{Name: "SAGE_MODE", Default: "inferred", Doc: "Operating mode: `standalone`, `fleet` or " +
		"`meta`; inferred from the config when unset"},
	{Name: "SAGE_DATABASE_URL", Default: envNone, File: true,
		Doc: "PostgreSQL connection string of the monitored database"},
	{Name: "SAGE_PG_HOST", Default: envCode("localhost"), Doc: "PostgreSQL host " +
		"(`postgres.host`)"},
	{Name: "SAGE_PG_PORT", Default: envCode(DefaultPGPort), Doc: "PostgreSQL port " +
		"(`postgres.port`), 1 to 65535"},
	{Name: "SAGE_PG_USER", Default: envCode(DefaultPGUser),
		Doc: "PostgreSQL user (`postgres.user`)"},
	{Name: "SAGE_PG_PASSWORD", Default: envNone, File: true,
		Doc: "PostgreSQL password (`postgres.password`)"},
	{Name: "SAGE_PG_DATABASE", Default: envCode(DefaultPGDatabase),
		Doc: "PostgreSQL database name (`postgres.database`)"},
	{Name: "SAGE_PG_SSLMODE", Default: envCode(DefaultPGSSLMode),
		Doc: "PostgreSQL sslmode (`postgres.sslmode`)"},
	{Name: "SAGE_PG_MAX_CONNS", Default: envCode(DefaultPGMaxConnections),
		Doc: "Max PostgreSQL connections in the pool (`postgres.max_connections`)"},
	{Name: "SAGE_META_DB", Default: envNone, File: true,
		Doc: "Metadata database connection string; selects meta-db fleet mode"},
	{Name: "SAGE_PROMETHEUS_PORT", Default: envCode(8187), Doc: "Port for Prometheus metrics, " +
		"1 to 65535; keeps the host of `prometheus.listen_addr` (default `127.0.0.1`)"},
	{Name: "SAGE_PROMETHEUS_ADDR", Default: envNone, Doc: "Whole `host:port` of the " +
		"separate `/metrics` listener (`prometheus.listen_addr`); wins over " +
		"`SAGE_PROMETHEUS_PORT` when both are set"},
	{Name: "SAGE_RATE_LIMIT", Default: envCode(DefaultRateLimit), Doc: "Max requests per " +
		"minute per IP on the REST API; zero or negative means the default"},
	{Name: "SAGE_API_TRUSTED_ORIGINS", Default: envNone, Doc: "Comma-separated " +
		"`api.trusted_origins`: browser origins allowed to send cross-origin API writes " +
		"([CSRF](security.md#csrf-and-cross-origin-requests))"},
	{Name: "SAGE_API_KEY", Default: envNone, File: true, Doc: "Legacy config field; the " +
		"current web/API path uses session login cookies"},
	{Name: "SAGE_TLS_CERT", Default: envNone, Doc: "PEM certificate (chain) file; with " +
		"`SAGE_TLS_KEY`, the API serves HTTPS only"},
	{Name: "SAGE_TLS_KEY", Default: envNone, Doc: "PEM private key file for `SAGE_TLS_CERT`; " +
		"set both or neither"},
	{Name: "SAGE_SUPERVISED", Default: envUnset, Doc: "`1` or `true` declares a supervisor " +
		"that relaunches the sidecar on exit code 42; only then does the dashboard's restart " +
		"endpoint work (otherwise it answers 501)"},
	{Name: "SAGE_ENCRYPTION_KEY", Default: envNone, File: true, Doc: "Passphrase that " +
		"encrypts stored secrets, including API-set config secrets " +
		"([secrets at rest](security.md#secrets-at-rest))"},
	{Name: "SAGE_ENCRYPTION_KEY_PREVIOUS", Default: envNone, File: true,
		Doc: "The previous passphrase during a key rotation"},
	{Name: "SAGE_DATA_DIR", Default: envNone, Doc: "Persistent directory; without an " +
		"encryption key, pg_sage generates `encryption.key` there " +
		"([secrets at rest](security.md#secrets-at-rest))"},
	{Name: "SAGE_BREAK_GLASS_PASSWORD_HASH", Default: envNone, File: true, Doc: "bcrypt hash " +
		"of the [break-glass admin](security.md#break-glass-admin) password"},
	{Name: "SAGE_BOOTSTRAP_ADMIN_EMAIL", Default: envNone, Doc: "Email of the first admin, " +
		"created only when the users table is empty"},
	{Name: "SAGE_BOOTSTRAP_ADMIN_PASSWORD", Default: envNone, File: true, Doc: "Password of " +
		"that first admin; its `_FILE` must be mode 600 or 400"},
	{Name: "SAGE_TRUST_LEVEL", Default: envCode(DefaultTrustLevel), Doc: "Operator trust " +
		"ceiling (`trust.level`): `observation`, `advisory` or `autonomous`"},
	{Name: "SAGE_POLICY_PROFILE", Default: envCode(DefaultPolicyProfile), Doc: "Bootstrap " +
		"standing-policy profile (`policy.profile`): `staffed` or `unattended`"},
	{Name: "SAGE_VALUE_TOIL_MODEL_VERSION", Default: envCode(DefaultToilModelVersion),
		Doc: "Active calibrated toil-model version (`value.toil_model_version`)"},
	{Name: "SAGE_LLM_API_KEY", Default: envNone, File: true,
		Doc: "API key for Gemini or any OpenAI-compatible LLM"},
	{Name: "SAGE_LLM_AUTH", Default: envCode("api_key"), Doc: "`databricks` uses SDK unified " +
		"authentication with token refresh for Unity Gateway"},
	{Name: "SAGE_LLM_ENDPOINT", Default: envNone, Doc: "LLM API endpoint (`llm.endpoint`)"},
	{Name: "SAGE_LLM_MODEL", Default: envNone, Doc: "LLM model name (`llm.model`)"},
	{Name: "SAGE_OPTIMIZER_LLM_API_KEY", Default: envNone, File: true,
		Doc: "Separate API key for the optimizer model (optional)"},
	{Name: "SAGE_OPTIMIZER_LLM_ENDPOINT", Default: envNone,
		Doc: "Separate endpoint for the optimizer model (optional)"},
	{Name: "SAGE_OPTIMIZER_LLM_MODEL", Default: envNone,
		Doc: "Separate model name for the optimizer (optional)"},
	{Name: "SAGE_LAKEBASE_ENDPOINT", Default: envNone, Doc: "Lakebase endpoint resource path " +
		"for OAuth; applies to target and metadata pools"},
	{Name: "SAGE_LAKEBASE_ENDPOINTS", Default: envNone, Doc: "JSON hostname-to-endpoint map " +
		"for a fleet spanning Lakebase endpoints; use instead of the singular variable"},
	{Name: "SAGE_AUTH_MODE", Default: envNone, Doc: "`databricks_app` selects Databricks Apps " +
		"authentication (see Web UI and API Authentication)"},
	{Name: "SAGE_DATABRICKS_ADMIN_EMAIL", Default: envNone, Doc: "Administrator email under " +
		"`SAGE_AUTH_MODE=databricks_app`; startup fails without a plain address"},
	{Name: "SAGE_DATABRICKS_ALLOWED_EMAIL_DOMAINS", Default: envNone, Doc: "Comma-separated " +
		"exact DNS domains allowed under `databricks_app` auth; the administrator must match"},
	{Name: "SAGE_DATABRICKS_APP_IDENTITY_PIN", Default: "log", Doc: "What a changed " +
		"`X-Forwarded-User` for a pinned Databricks App account does: `log` (audit event), " +
		"`enforce` (also refuse with 403) or `off`"},
	{Name: "SAGE_OAUTH_PROVIDER", Default: envNone, Doc: "OAuth provider (`oauth.provider`): " +
		"google, github, okta or generic OIDC"},
	{Name: "SAGE_OAUTH_CLIENT_ID", Default: envNone, Doc: "OAuth client ID (`oauth.client_id`)"},
	{Name: "SAGE_OAUTH_CLIENT_SECRET", Default: envNone, File: true,
		Doc: "OAuth client secret (`oauth.client_secret`)"},
	{Name: "SAGE_OAUTH_ISSUER_URL", Default: envNone,
		Doc: "OIDC issuer URL (`oauth.issuer_url`)"},
	{Name: "SAGE_OAUTH_REDIRECT_URL", Default: envNone, Doc: "OAuth redirect URL " +
		"(`oauth.redirect_url`); must match the URI registered at the provider"},
	{Name: "SAGE_MCP_ENABLED", Default: envCode(DefaultMCPEnabled), Doc: "Serve the MCP " +
		"endpoint (`mcp.enabled`); `false` or `0` turns it off"},
	{Name: "SAGE_MCP_TRANSPORT", Default: envCode(DefaultMCPTransport), Doc: "MCP transport " +
		"(`mcp.transport`): `http` (credential required) or `stdio`"},
	{Name: "SAGE_VERIFY_WINDOW_MINUTES", Default: envCode(DefaultVerifyWindowMinutes),
		Doc: "Initial verification window in minutes (`verify.window_minutes`)"},
	{Name: "SAGE_VERIFY_WINDOW_MAX_MINUTES", Default: envCode(DefaultVerifyWindowMaxMinutes),
		Doc: "Maximum verification window in minutes (`verify.window_max_minutes`)"},
	{Name: "SAGE_VERIFY_MIN_GAIN_PCT", Default: envCode(DefaultVerifyMinGainPct),
		Doc: "Minimum gain to retain a change (`verify.min_gain_pct`); `0` is allowed"},
	{Name: "SAGE_VERIFY_REGRESS_PCT", Default: envCode(DefaultVerifyRegressPct),
		Doc: "Regression percentage that triggers a revert (`verify.regress_pct`)"},
	{Name: "SAGE_VERIFY_WRITE_IMPACT_PCT", Default: envCode(DefaultVerifyWriteImpactPct),
		Doc: "Maximum rise of the table's WAL bytes per written tuple " +
			"(`verify.write_impact_pct`)"},
	{Name: "SAGE_VERIFY_MIN_SAMPLES", Default: envCode(DefaultVerifyMinSamples),
		Doc: "Minimum verification samples (`verify.min_samples`)"},
	{Name: "SAGE_CLONE_PROVIDER", Default: envCode(DefaultCloneProvider),
		Doc: "Clone provider (`clone.provider`): `none`, `dle`, `lakebase_branch`, " +
			"`neon_branch`, `aurora_clone` or `rds_restore`"},
	{Name: "SAGE_CLONE_DLE_ENDPOINT", Default: envNone,
		Doc: "Database Lab Engine API endpoint (`clone.dle_endpoint`)"},
	{Name: "SAGE_CLONE_DLE_TOKEN", Default: envNone, File: true,
		Doc: "Database Lab bearer token (`clone.dle_token`)"},
	{Name: "SAGE_CLONE_NEON_API_KEY", Default: envNone, File: true,
		Doc: "Neon API key for `neon_branch` rehearsal branches (`clone.neon.api_key`)"},
	{Name: "SAGE_CLONE_MAX_AGE_MINUTES", Default: envCode(DefaultCloneMaxAgeMinutes),
		Doc: "Maximum source snapshot age (`clone.max_clone_age_minutes`)"},
	{Name: "SAGE_CUSTODIAN_FREEZE_RED_BUFFER_PCT", Default: envCode(DefaultFreezeRedBufferPct),
		Doc: "Remaining XID percentage classified red (`custodian.freeze.red_buffer_pct`)"},
	{Name: "SAGE_CUSTODIAN_WAL_ABANDON_AFTER_MINUTES",
		Default: envCode(DefaultWALAbandonAfterMinutes),
		Doc: "Inactive replication-slot abandonment age " +
			"(`custodian.wal.abandon_after_minutes`)"},
	{Name: "SAGE_CUSTODIAN_WAL_DISK_PCT_CEILING",
		Default: envCode(DefaultWALRetainedDiskPctCeiling),
		Doc:     "Retained-WAL disk limit (`custodian.wal.retained_wal_disk_pct_ceiling`)"},
	{Name: "SAGE_CLOUD_TELEMETRY_ENABLED", Default: envUnset, Doc: "Read RDS, Aurora and " +
		"Cloud SQL host telemetry (`cloud_telemetry.enabled`); `true` or `false`"},
	{Name: "SAGE_AWS_REGION", Default: envNone,
		Doc: "AWS region when the host does not name it (`cloud_telemetry.aws.region`)"},
	{Name: "SAGE_AWS_DB_INSTANCE_IDENTIFIER", Default: envNone, Doc: "RDS DB instance " +
		"identifier when the host is not an RDS endpoint; standalone only"},
	{Name: "SAGE_AWS_DB_CLUSTER_IDENTIFIER", Default: envNone, Doc: "Aurora DB cluster " +
		"identifier when the host is not an Aurora endpoint; standalone only"},
	{Name: "SAGE_GCP_PROJECT", Default: envNone,
		Doc: "Google Cloud project of the Cloud SQL instance (`cloud_telemetry.gcp.project`)"},
	{Name: "SAGE_GCP_CLOUDSQL_INSTANCE", Default: envNone, Doc: "Cloud SQL instance name " +
		"when the host is not its IP address; standalone only"},
	{Name: "SAGE_AZURE_SUBSCRIPTION_ID", Default: envNone,
		Doc: "Azure subscription ID of the flexible server (`azure.subscription_id`)"},
	{Name: "SAGE_AZURE_RESOURCE_GROUP", Default: envNone,
		Doc: "Azure resource group of the flexible server (`azure.resource_group`)"},
	{Name: "SAGE_AZURE_SERVER_NAME", Default: envNone, Doc: "Flexible server name for hosts " +
		"that do not name it (`azure.server_name`)"},
	{Name: "SAGE_SUPABASE_OBSERVABILITY_TOKEN", Default: envNone, File: true,
		Doc: "Supabase Management API token for provider observability (optional)"},
	{Name: "SAGE_HISTORY_MONITORED_DSN", Default: envNone, File: true, Doc: "Source database " +
		"of the `history` subcommand's copy into the metadata database (only read)"},
	{Name: "SAGE_VECTORLAB_DATABASE_URL", Default: envNone, File: true,
		Doc: "Database of the `vector-lab` subcommand"},
	{Name: "SAGE_LINT_DATABASE_URL", Default: envNone, File: true, Doc: "Monitored " +
		"database the `lint-migration` CI gate reads catalog statistics from (only read; " +
		"not needed with --static)"},
}

// contributorEnvVars are read only by tests (internal/testdb) and benches.
var contributorEnvVars = []EnvVar{
	{Name: "SAGE_TEST_DATABASE_URL", Default: envNone, Doc: "Disposable PostgreSQL server " +
		"for database tests; each test package creates and drops its own database on it. " +
		"Unset, database tests skip. Never point it at a shared server"},
	{Name: "SAGE_TEST_SECOND_CLUSTER_URL", Default: envNone, Doc: "A second disposable " +
		"PostgreSQL server, for tests that need a database on another cluster; unset, they skip"},
	{Name: "SAGE_TEST_EXCLUSIVE_URL", Default: envNone, Doc: "A separate disposable " +
		"PostgreSQL server for tests that measure or reset cluster-wide state; they run on it " +
		"one at a time. It must not be the `SAGE_TEST_DATABASE_URL` server (checked by " +
		"`system_identifier`). Unset, they skip, and the CI skip budget fails"},
	{Name: "SAGE_TEST_REPLICA_URL", Default: envNone, Doc: "Streaming replica of the " +
		"`SAGE_TEST_DATABASE_URL` server, for replica tests; unset, they skip"},
	{Name: "SAGE_TEST_DSN", Default: "(set by the fixture)", Doc: "Legacy test DSN name; " +
		"the fixture overwrites it with the package database, so do not set it"},
	{Name: "SAGE_BENCH_RUN", Default: envUnset,
		Doc: "`1` runs `TestPGIncidentBench` in `sre-bench`"},
	{Name: "SAGE_BENCH_REPEATS", Default: envCode(1),
		Doc: "Runs of every PGIncidentBench scenario, 1 to 10"},
	{Name: "SAGE_BENCH_MIN_RUNS_PER_FAMILY", Default: envUnset,
		Doc: "Scored top-1 runs every PGIncidentBench family needs: sizes the repeats, " +
			"tops up short families and fails a family still short"},
	{Name: "SAGE_BENCH_EXPECTED", Default: envUnset,
		Doc: "PGIncidentBench expected-scenario manifest; a skip it does not allow fails the run"},
	{Name: "SAGE_BENCH_REPLAY", Default: envUnset,
		Doc: "`1` also replays the PGIncidentBench corpus (CI: the core shard only)"},
	{Name: "SAGE_BENCH_REPORT_DIR", Default: "(a temporary directory)",
		Doc: "Directory that receives the PGIncidentBench JSON and Markdown reports"},
	{Name: "SAGE_BENCH_FAMILIES", Default: "(all)", Doc: "Comma-separated trigger kinds a " +
		"bench run is limited to, such as `checkpoint_storm,lwlock_contention`"},
	{Name: "SAGE_BENCH_SPLIT", Default: envCode("all"), Doc: "Replayed cases: `all`, " +
		"`held_out` or `tuning`"},
	{Name: "SAGE_BENCH_LIVE_ARM", Default: envUnset, Doc: "`1` runs `TestLiveModelArm`, " +
		"which calls the configured LLM"},
	{Name: "SAGE_BENCH_LIVE_MODEL_ARM", Default: envCode("causal-graph+llm"), Doc: "Model " +
		"arm a live run measures: `causal-graph+llm` or `causal-graph+investigator`"},
	{Name: "SAGE_BENCH_PG_SAGE_VERSION", Default: envNone, Doc: "Release tag stamped into " +
		"bench reports (a leading `v` is dropped); CI sets it on tags"},
	{Name: "SAGE_BENCH_PG_SAGE_COMMIT", Default: envNone, Doc: "Commit stamped into bench " +
		"reports, so a sidecar refuses a report made for another build"},
	{Name: "SAGE_SAFETY_BENCH_RUN", Default: envUnset,
		Doc: "`1` runs the full `TestAgentSafetyBench` in `safety-bench`"},
	{Name: "SAGE_SAFETY_BENCH_REPORT_DIR", Default: "(a temporary directory)",
		Doc: "Directory that receives the agent-safety bench JSON and Markdown reports"},
}

// EnvVars returns a copy of the registry, in documentation order.
func EnvVars() []EnvVar {
	return append([]EnvVar(nil), envRegistry...)
}
