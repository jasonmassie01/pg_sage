import { describe, expect, it } from 'vitest'
import { canRollBackRow, rollbackButtonLabel, showsRollbackReason } from './ledger'

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

  // The identity check abandoned the revert: the index is not pg_sage's.
  it('refuses a retry of an abandoned revert', () => {
    const reason = 'revert refused: index public.idx_a is no longer the object pg_sage created'
    expect(canRollBackRow(row('rollback_skipped', { rollback_reason: reason }))).toBe(false)
    expect(canRollBackRow(row('rollback_skipped', {
      rollback_reason: 'emergency stop active; automatic rollback withheld',
    }))).toBe(true)
    expect(canRollBackRow(row('rollback_failed', { rollback_reason: reason }))).toBe(true)
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

describe('showsRollbackReason', () => {
  it.each(['rolled_back', 'rollback_skipped', 'rollback_failed'])(
    'shows the reason of a %s row', outcome => {
      expect(showsRollbackReason(row(outcome, { rollback_reason: 'why' }))).toBe(true)
    })

  it('hides an empty reason', () => {
    expect(showsRollbackReason(row('rollback_failed', { rollback_reason: '' }))).toBe(false)
  })

  // failed rows have their own error box; success rows have no reason.
  it.each(['failed', 'success'])('leaves %s to the other boxes', outcome => {
    expect(showsRollbackReason(row(outcome, { rollback_reason: 'why' }))).toBe(false)
  })
})
