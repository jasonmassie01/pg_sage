import { describe, expect, it } from 'vitest'
import { canRollBackRow, rollbackButtonLabel } from './ledger'

const row = (outcome, overrides = {}) => ({
  record_kind: 'executed', outcome,
  rollback_sql: 'DROP INDEX CONCURRENTLY IF EXISTS public.idx_a', ...overrides,
})

describe('canRollBackRow', () => {
  it.each(['success', 'monitoring', 'pending'])('allows a %s row with SQL', outcome => {
    expect(canRollBackRow(row(outcome))).toBe(true)
  })

  // executor-07: the backend retries both; the button was missing exactly
  // when the operator needed it.
  it.each(['rollback_skipped', 'rollback_failed'])('allows a retry of %s', outcome => {
    expect(canRollBackRow(row(outcome))).toBe(true)
  })

  it.each(['rollback_skipped', 'rollback_failed', 'success'])(
    'refuses %s without rollback SQL', outcome => {
      expect(canRollBackRow(row(outcome, { rollback_sql: '' }))).toBe(false)
      expect(canRollBackRow(row(outcome, { rollback_sql: null }))).toBe(false)
    })

  it.each(['rolled_back', 'rolling_back', 'failed', 'already_restored', '', undefined])(
    'refuses %s', outcome => {
      expect(canRollBackRow(row(outcome))).toBe(false)
    })

  it('refuses a queued proposal even with SQL', () => {
    expect(canRollBackRow(row('rollback_failed', { record_kind: 'queued' }))).toBe(false)
  })
})

describe('rollbackButtonLabel', () => {
  it.each(['rollback_skipped', 'rollback_failed'])('labels %s as a retry', outcome => {
    expect(rollbackButtonLabel(row(outcome))).toBe('Retry rollback')
  })

  it.each(['success', 'monitoring', 'pending'])('labels %s as a first rollback', outcome => {
    expect(rollbackButtonLabel(row(outcome))).toBe('Roll Back Action')
  })
})
