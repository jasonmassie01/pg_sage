// providers: the setup wizard's provider presets (ux-04b). detectProvider
// follows ux-04a's host rules (internal/doctor/provider.go platformFromHost);
// roleSQL is the role setup each provider's admin runs (docs/installation.md).

export const PROVIDERS = Object.freeze([
  { provider: 'rds', label: 'Amazon RDS', admin: 'rds_superuser' },
  { provider: 'aurora', label: 'Amazon Aurora', admin: 'rds_superuser' },
  { provider: 'cloud-sql', label: 'Google Cloud SQL', admin: 'cloudsqlsuperuser' },
  { provider: 'alloydb', label: 'Google AlloyDB', admin: 'alloydbsuperuser' },
  { provider: 'azure', label: 'Azure Database for PostgreSQL', admin: 'azure_pg_admin' },
  { provider: 'neon', label: 'Neon', admin: 'neon_superuser' },
  { provider: 'supabase', label: 'Supabase', admin: 'supabase_admin' },
  { provider: 'lakebase', label: 'Databricks Lakebase', admin: 'databricks_superuser' },
  { provider: 'self-managed', label: 'Self-managed PostgreSQL', admin: 'superuser' },
])

const LAKEBASE_HOST =
  /^[a-z0-9-]+\.database\.[a-z0-9-]+\.(cloud\.databricks\.com|azuredatabricks\.net)$/

const HOST_RULES = [
  [h => LAKEBASE_HOST.test(h), 'lakebase'],
  [h => h.endsWith('.neon.tech'), 'neon'],
  [h => h.endsWith('.supabase.co') || h.endsWith('.pooler.supabase.com'), 'supabase'],
  [h => h.endsWith('.postgres.database.azure.com') ||
    h.endsWith('.postgres.cosmos.azure.com'), 'azure'],
  [h => h.endsWith('.rds.amazonaws.com') && h.includes('.cluster-'), 'aurora'],
  [h => h.endsWith('.rds.amazonaws.com'), 'rds'],
]

// detectProvider names the provider a host belongs to, or 'unknown' (a
// Cloud SQL or AlloyDB address does not say, so those are chosen by hand).
export function detectProvider(host) {
  const h = String(host ?? '').trim().toLowerCase().replace(/\.$/, '')
  if (h === '' || /[/:@\\\s]/.test(h)) return 'unknown'
  return HOST_RULES.find(([match]) => match(h))?.[1] ?? 'unknown'
}

// usesOAuth: Lakebase signs in with Databricks OAuth, not a password.
export function usesOAuth(provider) {
  return provider === 'lakebase'
}

// quoteRole quotes a role that is not a plain lower-case identifier.
function quoteRole(role) {
  if (/^[a-z_][a-z0-9_$]*$/.test(role)) return role
  return `"${role.replace(/"/g, '""')}"`
}

function preset(provider) {
  return PROVIDERS.find(p => p.provider === provider) ??
    PROVIDERS[PROVIDERS.length - 1]
}

// roleSQL is the setup the provider's admin runs for pg_sage's role. It
// carries a placeholder, never a real password.
export function roleSQL(provider, role) {
  const p = preset(provider)
  const r = quoteRole(String(role ?? '').trim() || 'sage_agent')
  const create = usesOAuth(p.provider)
    ? ['-- Lakebase signs in with Databricks OAuth: create the role for the service',
      '-- principal in the Lakebase console (no password), then run:']
    : [`CREATE USER ${r} WITH PASSWORD 'YOUR_PASSWORD';`]
  return [
    `-- Run as the ${p.label} admin (${p.admin}).`,
    ...create,
    `GRANT pg_monitor TO ${r};`,
    `GRANT pg_read_all_stats TO ${r};`,
    `GRANT pg_signal_backend TO ${r};`,
    'CREATE EXTENSION IF NOT EXISTS pg_stat_statements;',
    `CREATE SCHEMA IF NOT EXISTS sage AUTHORIZATION ${r};`,
  ].join('\n')
}
