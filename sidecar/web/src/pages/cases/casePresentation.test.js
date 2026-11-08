import { describe, expect, it } from 'vitest'
import { actionAvailability, caseID, caseLink, casePresentation } from './casePresentation'

function finding(category, detail = {}, overrides = {}) {
  return { id: 'finding:prod:example', database_name: 'prod',
    identity_key: `finding:prod:${category}:query:queryid:123`,
    title: 'Original technical title', severity: 'warning',
    why_now: 'not urgent', evidence: [{ detail }], ...overrides }
}

describe('Case presentation grounded in reported evidence', () => {
  it('separates measured usage from a qualified consequence and unknown urgency', () => {
    const summary = casePresentation(finding('sequence_exhaustion', { usage_pct: 94.99 },
      { why_now: 'critical severity requires immediate review' }))
    expect(summary.title).toBe('An ID counter is nearly full')
    expect(summary.observed).toBe('This sequence has used about 95% of its reported range.')
    expect(summary.consequence).toContain('can fail')
    expect(summary.consequence).toContain('Time to limit and affected applications have not been established')
    expect(summary.attention).toContain('Urgency not assessed')
  })

  it('formats the known interval deterministically without inventing a byte size', () => {
    const summary = casePresentation(finding('temp_spill', {
      delta_temp_blks_written: 2834, interval_seconds: 59.997186346,
    }))
    expect(summary.title).toBe('A query used temporary disk space')
    expect(summary.observed).toBe('2,834 temporary blocks were written during an approximately 60-second collection interval.')
    expect(summary.consequence).toContain('effect on users has not been established')
    expect(summary.consequence).toContain('No memory setting has been validated')
    expect(summary.observed).not.toMatch(/bytes|MB|GB/)
  })

  it('does not manufacture measurements when counters or interval are missing', () => {
    expect(casePresentation(finding('sequence_exhaustion')).observed).toContain('unavailable')
    expect(casePresentation(finding('temp_spill', { delta_temp_blks_written: 128 })).observed)
      .toContain('duration unknown')
    expect(casePresentation(finding('temp_spill')).observed).toContain('count is unavailable')
    expect(casePresentation(finding('unknown')).attention).toContain('Urgency not assessed')
  })

  it('describes old row estimates without equating their ratio to recoverable disk space', () => {
    const summary = casePresentation(finding('table_bloat', {
      n_live_tup: 10000, n_dead_tup: 15000, dead_ratio: 0.6, io_saturated: true,
    }))
    expect(summary.title).toBe('A table has old row versions awaiting cleanup')
    expect(summary.observed).toBe('Reported estimates: 10,000 live rows; 15,000 old row versions (about 60% old versions).')
    expect(summary.consequence).toContain('recoverable disk space and effect on users have not been established')
    expect(summary.consequence).toContain('high disk I/O pressure')
    expect(casePresentation(finding('table_bloat')).observed).toContain('count unavailable')
    expect(casePresentation(finding('table_bloat', { dead_ratio: 3 })).observed).not.toContain('%')
  })

  it('identifies scan counts as counters with an unknown window, not a current rate or proof of a missing index', () => {
    const summary = casePresentation(finding('seq_scan_heavy', {
      seq_scan: 1034, idx_scan: 0, n_live_tup: 50000,
    }))
    expect(summary.title).toBe('A table is often read without using an index')
    expect(summary.observed).toContain('1,034 sequential scans (reading table rows directly); 0 index scans')
    expect(summary.observed).toContain('counter window is unknown')
    expect(summary.observed).toContain('Reported live-row estimate: 50,000')
    expect(summary.consequence).toContain('a sequential scan can be appropriate')
    expect(casePresentation(finding('seq_scan_heavy')).observed).toContain('count unavailable')
  })

  it('describes invalid status without inventing a cause or treating removal as a fix', () => {
    const summary = casePresentation(finding('invalid_index', {
      table: 'orders', index_def: 'CREATE INDEX invalid_orders ON orders(id)',
    }))
    expect(summary.title).toBe('An index is marked invalid')
    expect(summary.observed).toContain('invalid on table orders')
    expect(summary.consequence).toContain('cause and effect on users have not been established')
    expect(summary.consequence).toContain('review its definition and dependencies')
    expect(casePresentation(finding('invalid_index')).observed).toContain('affected table is not supplied')
  })

  it('provides short glossary explanations for first-use database terms', () => {
    expect(casePresentation(finding('sequence_exhaustion')).glossary.definition).toContain('automatic counter')
    expect(casePresentation(finding('invalid_index')).glossary.definition).toContain('lookup structure')
    expect(casePresentation(finding('stale_statistics')).glossary.definition).toContain('Estimates')
  })

  it('distinguishes an executor limitation from a database feature limitation', () => {
    const row = finding('duplicate_index', {}, { action_candidates: [{
      action_type: 'drop_unused_index', blocked_reason: 'provider_unsupported',
    }] })
    expect(actionAvailability(row)).toContain('pg_sage cannot run this proposed action for this connection')
    expect(actionAvailability(row)).not.toContain('database does not support')
    expect(actionAvailability({ ...row, action_candidates: [{ policy_decision: {
      decision: 'blocked', blocked_reason: 'observation mode',
    } }] })).toContain('changes are disabled in Observation mode')
    expect(actionAvailability({ ...row, action_candidates: [] })).toContain('No proposed change is attached')
  })

  it('keeps case and database identity across all scoped routes', () => {
    const row = { case_id: 'finding:db one:q/a?b', database_name: 'db one' }
    for (const page of ['cases', 'ask', 'actions']) {
      const [route, query] = caseLink(page, row).split('?')
      expect(route).toBe(`#/${page}`)
      expect(new URLSearchParams(query).get('case')).toBe(row.case_id)
      expect(new URLSearchParams(query).get('database')).toBe(row.database_name)
    }
  })

  it('uses the same canonical case alias for focused, Ask and Actions links', () => {
    for (const row of [
      { case_id: 'alias-only', database_name: 'prod' },
      { id: 'id-only', database_name: 'prod' },
      { case_id: 'preferred-alias', id: 'legacy-id', database_name: 'prod' },
    ]) {
      expect(caseID(row)).toBe(row.case_id || row.id)
      for (const page of ['cases', 'ask', 'actions']) {
        expect(new URLSearchParams(caseLink(page, row).split('?')[1]).get('case'))
          .toBe(row.case_id || row.id)
      }
    }
  })
})
