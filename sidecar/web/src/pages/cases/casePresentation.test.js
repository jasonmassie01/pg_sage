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

  it('states an assessed time to impact with its basis (cases-07)', () => {
    const summary = casePresentation(finding('storage_forecast', {}, { urgency: {
      assessed: true, time_to_impact_seconds: 259200, impact: 'until full',
      basis: 'days_until_full 3 from a 30-point regression, R² 0.94',
    } }))
    expect(summary.attention).toBe('About 3 days until full (days_until_full 3 from a 30-point regression, R² 0.94)')
    const soon = casePresentation(finding('forecast_wal_runway', {}, { urgency: {
      assessed: true, time_to_impact_seconds: 57600, impact: 'until the disk is full', basis: 'b',
    } }))
    expect(soon.attention).toBe('About 16 hours until the disk is full (b)')
    const minutes = casePresentation(finding('forecast_wal_runway', {}, { urgency: {
      assessed: true, time_to_impact_seconds: 20, impact: 'until the disk is full', basis: 'b',
    } }))
    expect(minutes.attention).toBe('About 1 minute until the disk is full (b)')
  })

  it('never shows a number for an urgency without a usable time to impact', () => {
    for (const tti of [0, -5, null, undefined, '3', Number.NaN, Infinity]) {
      const summary = casePresentation(finding('storage_forecast', {}, {
        why_now: 'Urgency not assessed', urgency: { assessed: true, time_to_impact_seconds: tti },
      }))
      expect(summary.attention).toContain('Urgency not assessed')
      expect(summary.attention).not.toMatch(/About|\d/)
    }
  })

  it('shows measured headroom while saying urgency is not assessed', () => {
    const summary = casePresentation(finding('xid_wraparound', {}, { urgency: {
      assessed: false, basis: 'xid_age 1.2B of the 1.6B critical threshold',
    } }))
    expect(summary.attention).toContain('Urgency not assessed: xid_age 1.2B of the 1.6B critical threshold.')
    expect(summary.attention).not.toContain('About')
  })

  it('says a limit already reached without showing a number', () => {
    const summary = casePresentation(finding('forecast_wal_runway', {}, {
      why_now: 'Urgency not assessed: seconds_to_limit is zero, so the producer reports the limit already reached',
      urgency: { assessed: false, limit_reached: true },
    }))
    expect(summary.attention).toBe('Urgency not assessed: seconds_to_limit is zero, so the producer reports the limit already reached')
    expect(summary.attention).not.toMatch(/About|\d/)
  })

  it('describes a recurrence with its count and the previous resolution', () => {
    const row = { id: 'incident:prod:deterministic:c', database_name: 'prod', title: 'x',
      recurrence: { previous_id: 'b', count: 4, previous_resolved_at: '2026-11-02T10:00:00Z' } }
    expect(casePresentation(row).recurrence).toBe('4th occurrence; previous resolved 2026-11-02')
    const ordinals = { 2: '2nd', 3: '3rd', 11: '11th', 12: '12th', 13: '13th', 21: '21st', 22: '22nd', 101: '101st' }
    for (const [count, word] of Object.entries(ordinals)) {
      expect(casePresentation({ ...row, recurrence: { ...row.recurrence, count: Number(count) } })
        .recurrence).toBe(`${word} occurrence; previous resolved 2026-11-02`)
    }
    expect(casePresentation({ ...row, recurrence: { previous_id: 'b', count: 2 } }).recurrence)
      .toBe('2nd occurrence; previous resolution time unknown')
  })

  it('discloses an unavailable recurrence history rather than a first occurrence', () => {
    const row = { id: 'incident:prod:x', title: 'x' }
    expect(casePresentation(row).recurrence).toBeNull()
    expect(casePresentation({ ...row, recurrence: { previous_id: 'b', history_unavailable: true } })
      .recurrence).toBe('Recurring incident; occurrence history unavailable')
    expect(casePresentation({ ...row, recurrence: { previous_id: 'b', count: 'x' } })
      .recurrence).toBe('Recurring incident; occurrence history unavailable')
  })
})

describe('Case next step reflects the recommendation lifecycle (cases-10)', () => {
  const candidate = (extra = {}) => ({ action_type: 'analyze', risk_tier: 'safe', ...extra })
  const withRec = (state, nextStep, extra = {}, cand = candidate()) => finding('stale_statistics', {}, {
    action_candidates: [cand],
    recommendation: { id: 9, state, revision: 1, next_step: nextStep, current_content: true, ...extra },
  })

  it('says pg_sage applied and reverted the change instead of calling it attached', () => {
    const text = 'pg_sage applied this on 2026-11-10 and reverted it: p95 latency rose 40%'
    const row = withRec('reverted', text, {}, candidate({ blocked_reason: 'recommendation_reverted' }))
    expect(actionAvailability(row)).toBe(text)
    expect(actionAvailability(row)).not.toContain('attached')
  })

  it('shows each known head state with its own next step', () => {
    const steps = {
      reverted: 'pg_sage applied this on 2026-11-10 and reverted it: hurt',
      abandoned: 'Stopped after 3 failed attempts',
      failed: 'Retrying after 14:05 UTC',
      superseded: 'Superseded: the analyzer no longer proposes this change',
      verified: 'pg_sage applied this on 2026-11-10 and verified it',
    }
    const seen = new Set()
    for (const [state, text] of Object.entries(steps)) {
      const shown = actionAvailability(withRec(state, text))
      expect(shown).toBe(text)
      seen.add(shown)
    }
    expect(seen.size).toBe(5)
  })

  it('keeps the attached wording for a proposal awaiting approval', () => {
    expect(actionAvailability(withRec('proposed', 'Proposed; awaiting approval')))
      .toContain('A proposed change is attached')
  })

  it('renders a state from a future version as unknown, never as attached', () => {
    const shown = actionAvailability(withRec('rolled_forward', 'A future sentence'))
    expect(shown).toContain('state unknown')
    expect(shown).not.toContain('attached')
    expect(shown).not.toContain('A future sentence')
  })

  it('says evidence expired rather than offering a stale proposal', () => {
    const row = finding('stale_statistics', {}, {
      action_candidates: [candidate({ blocked_reason: 'evidence_expired', expires_at: '2026-11-10T09:00:00Z' })],
    })
    expect(actionAvailability(row)).toBe('Evidence expired; waiting for a fresh observation')
  })

  it('discloses an unreadable recommendation history instead of saying none', () => {
    const row = finding('stale_statistics', {}, {
      action_candidates: [candidate()], recommendation_unknown: true,
    })
    const shown = actionAvailability(row)
    expect(shown).toContain('could not be read')
    expect(shown).not.toContain('attached')
    expect(actionAvailability({ ...row, action_candidates: [] })).toContain('could not be read')
  })

  it('treats a missing or empty recommendation as no lifecycle', () => {
    expect(actionAvailability(finding('x', {}, { action_candidates: [candidate()], recommendation: null })))
      .toContain('A proposed change is attached')
    expect(actionAvailability(withRec('failed', '')))
      .toContain('A proposed change is attached')
  })
})
