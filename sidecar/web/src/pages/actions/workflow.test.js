import { describe, expect, it } from 'vitest'
import { actionFamily, proposedChanges, recoveryForRevision } from './workflow'

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
