import { describe, expect, it } from 'vitest'
import {
  actionFamily, blockerText, hasReasonLabel, proposedChanges, reasonText, recoveryForRevision,
  verdictLabel,
} from './workflow'
import gateValues from './gateValues.json'

const issue = ({ database = 'prod', finding = '7', id = 'case-7', action = 'create_index_concurrently', candidate = {} } = {}) => ({
  id, database_name: database, source_type: 'finding', source_ids: [finding],
  action_candidates: [{ action_type: action, risk_tier: 'moderate', ...candidate }],
})
const stored = (overrides = {}) => ({ id: 3, revision: 1, database_name: 'prod', finding_id: 7,
  state: 'proposed', action_type: 'create_index', ...overrides })

const aliases = [['create_index', 'create_index_concurrently'], ['analyze', 'analyze_table'],
  ['vacuum', 'vacuum_table'], ['drop_index', 'drop_unused_index'], ['reindex', 'reindex_concurrently']]

describe('proposal presentation aliases', () => {
  it.each(aliases)('groups %s and %s only for the same explicit source', (headType, candidateType) => {
    const current = issue({ action: candidateType })
    const [result, ...extra] = proposedChanges([stored({ action_type: headType })], [current], 'prod')
    expect(extra).toHaveLength(0)
    expect(actionFamily(headType)).toBe(actionFamily(candidateType))
    expect(result).toMatchObject({ id: 3, revision: 1, action_type: headType,
      storedRecommendation: true, currentCandidate: { action_type: candidateType } })
  })

  it('preserves stored risk and revision separately from current proposal context', () => {
    const [result] = proposedChanges([stored({ risk_tier: 'safe', rollback_class: 'reversible' })],
      [issue({ candidate: { risk_tier: 'high_risk', rollback_class: 'unknown' } })], 'prod')
    expect(result).toMatchObject({ revision: 1, risk_tier: 'safe', rollback_class: 'reversible',
      currentCandidate: { risk_tier: 'high_risk', rollback_class: 'unknown' } })
  })

  it.each([
    { database: 'other', finding: '7' },
    { database: 'prod', finding: '8' },
  ])('does not merge different source identities: %j', ({ database, finding }) => {
    const results = proposedChanges([stored()], [issue({ database, finding })], 'prod')
    expect(results).toHaveLength(2)
    expect(results[0].linkedCase).toBeUndefined()
    expect(results[0].currentCandidate).toBeUndefined()
  })

  it('uses an explicit case ID without replacing it with a different finding match', () => {
    const current = issue()
    expect(proposedChanges([stored({ case_id: 'case-7', finding_id: null })], [current], 'prod'))
      .toHaveLength(1)
    expect(proposedChanges([stored({ case_id: 'different-case' })], [current], 'prod'))
      .toHaveLength(2)
  })

  it('does not treat similar unlisted action names as aliases', () => {
    const results = proposedChanges([stored({ action_type: 'alter' })], [issue({ action: 'alter_table' })], 'prod')
    expect(results).toHaveLength(2)
  })

  it('preserves distinct stored recommendation identities and revisions', () => {
    const results = proposedChanges([stored(), stored({ id: 4, revision: 2 })], [issue()], 'prod')
    expect(results).toHaveLength(2)
    expect(results.map(r => [r.id, r.revision])).toEqual([[3, 1], [4, 2]])
  })
})

describe('stored recovery presentation', () => {
  const forward = 'CREATE INDEX CONCURRENTLY idx_orders ON orders(customer_id)'
  const inverse = 'DROP INDEX CONCURRENTLY idx_orders;'
  const current = { proposed_sql: forward, rollback_class: 'reversible',
    script_output: { rollback_sql: inverse } }

  it('withholds an old inverse when current recovery is unknown even for identical forward SQL', () => {
    const result = recoveryForRevision({ currentCandidate: { ...current,
      rollback_class: 'unknown', script_output: {} } }, { forward_sql: forward, inverse_sql: 'DROP INDEX CONCURRENTLY ON;' })
    expect(result.sql).toBe('')
    expect(result.reason).toContain('validated reversible recovery plan has not been established')
  })

  it('withholds an inverse when the stored and current procedures differ', () => {
    expect(recoveryForRevision({ currentCandidate: current }, {
      forward_sql: 'CREATE INDEX different ON orders(customer_id)', inverse_sql: inverse,
    }).sql).toBe('')
    expect(recoveryForRevision({ currentCandidate: current }, {
      forward_sql: forward, inverse_sql: 'DROP INDEX CONCURRENTLY ON;',
    }).sql).toBe('')
  })

  it('withholds a stored inverse when no current case procedure is available', () => {
    expect(recoveryForRevision({}, { forward_sql: forward, inverse_sql: inverse }).sql).toBe('')
  })

  it('shows a stored inverse only when both statements match a current reversible procedure', () => {
    expect(recoveryForRevision({ currentCandidate: current }, {
      forward_sql: forward, inverse_sql: inverse,
    })).toEqual({ sql: inverse, reason: '' })
  })
})

// ux-05b wins 6 and 8: the gate's verdicts and reasons read as words, each
// held-back reason with a next step. gateValues.json is generated from the
// Go constants (internal/executor/gate_values_test.go), so a new reason
// constant fails the Go test until the list is regenerated, then fails
// here until it has a label. No concurrent access tests: pure functions.
describe('gate labels', () => {
  const rawEnum = /\b[a-z0-9]+_[a-z0-9_]+\b/

  // The product's own name is the one snake_case word a label may carry.
  const withoutProductName = text => text.replaceAll('pg_sage', 'pg-sage')

  it('labels every reason the gate can record, never as the bare enum', () => {
    expect(gateValues.reasons.length).toBeGreaterThan(50)
    for (const reason of gateValues.reasons) {
      expect(hasReasonLabel(reason), reason).toBe(true)
      const text = blockerText(reason)
      expect(withoutProductName(text), reason).not.toMatch(rawEnum)
      expect(text.toLowerCase(), reason).not.toBe(reason.replaceAll('_', ' '))
    }
  })

  it('labels every verdict spelling the gate, ledger and executor serve', () => {
    expect(gateValues.verdicts).toEqual(expect.arrayContaining(
      ['park', 'parked', 'queue_approval', 'queue_for_approval']))
    for (const verdict of gateValues.verdicts) {
      expect(verdictLabel(verdict), verdict).not.toMatch(/^Unknown verdict/)
      expect(verdictLabel(verdict), verdict).not.toMatch(rawEnum)
    }
  })

  it('names the held-back verdicts as the filter does', () => {
    expect(verdictLabel('queue_approval')).toBe('Waiting for approval')
    expect(verdictLabel('queue_for_approval')).toBe('Waiting for approval')
    expect(verdictLabel('parked')).toBe('Parked')
    expect(verdictLabel('park')).toBe('Parked')
    expect(verdictLabel('observe_only')).toBe('Observe only')
  })

  it('gives the five held-back reasons a next step', () => {
    expect(blockerText('emergency_stop'))
      .toBe('Emergency stop is on. Release it in the header to resume.')
    expect(blockerText('budget_exceeded')).toMatch(/budget exceeded\. .*budget frees/i)
    expect(blockerText('awaiting_verification')).toMatch(/verif.*\. .*retried/i)
    expect(blockerText('replica_mutation')).toMatch(/replica.*\. .*primary/i)
    expect(blockerText('provider_managed_setting')).toMatch(/provider.*\. .*console/i)
  })

  it('keeps the labels it had', () => {
    expect(blockerText('observation_mode'))
      .toBe('Database changes are disabled in Observation mode.')
    expect(blockerText('maintenance_window')).toBe('Waiting for an allowed maintenance window.')
  })

  it('labels the case restrictions a candidate can carry (cases-10)', () => {
    expect(blockerText('evidence_expired'))
      .toBe('Evidence expired; waiting for a fresh observation.')
    expect(blockerText('recommendation_reverted')).toMatch(/reverted .*not propose it again/)
    expect(blockerText('recommendation_abandoned')).toMatch(/retries .*not propose it again/)
  })

  it('names an unknown code as unknown, keeping the code readable', () => {
    expect(blockerText('made_up_reason')).toBe('Unknown reason (made_up_reason)')
    expect(hasReasonLabel('made_up_reason')).toBe(false)
    expect(verdictLabel('maybe_later')).toBe('Unknown verdict (maybe_later)')
    expect(verdictLabel('<b>x</b>')).toBe('Unknown verdict (<b>x</b>)')
  })

  it('passes a reason written as a sentence through unchanged', () => {
    const sentence = 'IO is saturated; wait for maintenance window or lower load'
    expect(blockerText(sentence)).toBe(sentence)
    expect(reasonText(sentence)).toBe(sentence)
  })

  it('does not inherit labels from the object prototype', () => {
    expect(hasReasonLabel('constructor')).toBe(false)
    expect(hasReasonLabel('toString')).toBe(false)
    expect(blockerText('constructor')).toBe('Unknown reason (constructor)')
    expect(verdictLabel('constructor')).toBe('Unknown verdict (constructor)')
  })

  it('says no reason or verdict was recorded for an empty value', () => {
    expect(blockerText('')).toBe('')
    expect(blockerText(undefined)).toBe('')
    expect(reasonText('')).toBe('No reason recorded.')
    expect(reasonText(null)).toBe('No reason recorded.')
    expect(reasonText('  ')).toBe('No reason recorded.')
    expect(verdictLabel('')).toBe('No verdict recorded')
    expect(verdictLabel(null)).toBe('No verdict recorded')
  })
})
