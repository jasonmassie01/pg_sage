import { describe, expect, it } from 'vitest'
import { PROVIDERS, detectProvider, roleSQL, usesOAuth } from './providers'

// ux-04b: the provider is detected from the host by ux-04a's rules
// (internal/doctor/provider.go platformFromHost) and each provider shows
// its role SQL. No concurrent access tests: pure functions.

describe('detectProvider', () => {
  it.each([
    ['orders.c9akciq32.us-east-1.rds.amazonaws.com', 'rds'],
    ['orders.cluster-c9akciq32.us-east-1.rds.amazonaws.com', 'aurora'],
    ['orders.postgres.database.azure.com', 'azure'],
    ['c-x.y.postgres.cosmos.azure.com', 'azure'],
    ['ep-cool-dawn-123456.us-east-2.aws.neon.tech', 'neon'],
    ['db.abcd.supabase.co', 'supabase'],
    ['aws-0-us-east-1.pooler.supabase.com', 'supabase'],
    ['instance-abc.database.us-east-1.cloud.databricks.com', 'lakebase'],
    ['instance-abc.database.eastus.azuredatabricks.net', 'lakebase'],
    ['ORDERS.POSTGRES.DATABASE.AZURE.COM.', 'azure'],
  ])('%s is %s', (host, want) => {
    expect(detectProvider(host)).toBe(want)
  })

  it.each(['10.0.0.5', 'db.internal', '', undefined, 'evil.com/x.neon.tech',
    'a b.neon.tech', 'neon.tech.evil.com'])('%j is unknown', host => {
    expect(detectProvider(host)).toBe('unknown')
  })
})

describe('roleSQL', () => {
  it('grants pg_monitor and the sage schema to the role for every provider', () => {
    for (const p of PROVIDERS) {
      const sql = roleSQL(p.provider, 'sage_agent')
      expect(sql, p.provider).toContain('GRANT pg_monitor TO sage_agent;')
      expect(sql, p.provider).toContain('CREATE SCHEMA IF NOT EXISTS sage AUTHORIZATION')
      expect(sql, p.provider).toContain(p.admin)
    }
  })

  it('names each managed admin role', () => {
    expect(roleSQL('rds', 'sage_agent')).toContain('rds_superuser')
    expect(roleSQL('cloud-sql', 'sage_agent')).toContain('cloudsqlsuperuser')
    expect(roleSQL('alloydb', 'sage_agent')).toContain('alloydbsuperuser')
    expect(roleSQL('azure', 'sage_agent')).toContain('azure_pg_admin')
  })

  it('creates no password for Lakebase, which signs in with Databricks OAuth', () => {
    expect(usesOAuth('lakebase')).toBe(true)
    expect(usesOAuth('rds')).toBe(false)
    const sql = roleSQL('lakebase', 'sp-role')
    expect(sql).not.toContain('PASSWORD')
    expect(sql).toContain('GRANT pg_monitor TO "sp-role";')
  })

  it('quotes a role that is not a plain identifier and never echoes a password', () => {
    expect(roleSQL('self-managed', 'Ops Team')).toContain('GRANT pg_monitor TO "Ops Team";')
    expect(roleSQL('self-managed', 'a"b')).toContain('"a""b"')
    expect(roleSQL('self-managed', 'sage_agent')).toContain("PASSWORD 'YOUR_PASSWORD'")
  })

  it('falls back to the self-managed text for an unknown provider and empty role', () => {
    const sql = roleSQL('unknown', '')
    expect(sql).toContain('GRANT pg_monitor TO sage_agent;')
    expect(sql).toContain('superuser')
  })
})
