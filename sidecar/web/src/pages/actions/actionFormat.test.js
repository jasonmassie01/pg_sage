import { afterEach, describe, expect, it } from 'vitest'
import { formatTime } from '../../lib/time'
import {
  actionCaseScope, actionRisk, actionRowKey, actionStatus, formatActionTime,
  lifecycleStatus, rollbackClassLabel, verificationStatus,
} from './actionFormat'

// dashboard-31: the helpers moved out of Actions.jsx. Each table covers
// null, empty string, an unknown value and every known value.
// No concurrent access tests: these are pure functions of their argument.

describe('actionStatus', () => {
  it.each([
    ['status first', { status: 'pending', action_status: 'x', outcome: 'y' }, 'pending'],
    ['action_status next', { action_status: 'approved', outcome: 'y' }, 'approved'],
    ['outcome last', { outcome: 'rolled_back' }, 'rolled_back'],
    ['empty strings fall through', { status: '', action_status: '', outcome: '' }, 'unknown'],
    ['null fields', { status: null, outcome: null }, 'unknown'],
    ['no fields', {}, 'unknown'],
    ['unknown value is kept', { status: 'mystery' }, 'mystery'],
  ])('%s', (_, row, want) => {
    expect(actionStatus(row)).toBe(want)
  })
})

describe('actionRisk', () => {
  it.each([
    ['risk_tier first', { risk_tier: 'high', action_risk: 'safe', risk: 'moderate' }, 'high'],
    ['action_risk next', { action_risk: 'safe', risk: 'moderate' }, 'safe'],
    ['risk last', { risk: 'moderate' }, 'moderate'],
    ['empty strings fall through', { risk_tier: '', action_risk: '', risk: '' }, 'unknown'],
    ['null fields', { risk_tier: null }, 'unknown'],
    ['no fields', {}, 'unknown'],
    ['unknown value is kept', { risk_tier: 'cosmic' }, 'cosmic'],
  ])('%s', (_, row, want) => {
    expect(actionRisk(row)).toBe(want)
  })
})

describe('verificationStatus', () => {
  it.each([
    ['improved verdict', { verification_outcome: { verdict: 'improved' } }, 'Improved'],
    ['neutral verdict', { verification_outcome: { verdict: 'neutral' } }, 'No change'],
    ['regressed verdict', { verification_outcome: { verdict: 'regressed' } }, 'Regressed'],
    ['insufficient evidence',
      { verification_outcome: { verdict: 'insufficient_evidence' } }, 'Inconclusive'],
    ['unverifiable', { verification_outcome: { verdict: 'unverifiable' } }, 'Unverifiable'],
    ['pending verdict', { verification_outcome: { verdict: 'pending' } }, 'Verifying'],
    ['unknown verdict', { verification_outcome: { verdict: 'odd' } }, 'odd'],
    ['empty verdict', { verification_outcome: { verdict: '' } }, 'Unknown'],
    ['outcome beats status',
      { verification_outcome: { verdict: 'neutral' }, verification_status: 'verified' }, 'No change'],
    ['status without outcome', { verification_status: 'verified' }, 'verified'],
    ['null outcome uses status', { verification_outcome: null, verification_status: 'x' }, 'x'],
    ['empty status', { verification_status: '' }, 'not_started'],
    ['no fields', {}, 'not_started'],
  ])('%s', (_, row, want) => {
    expect(verificationStatus(row)).toBe(want)
  })
})

describe('lifecycleStatus', () => {
  it.each([
    ['lifecycle_state first', { lifecycle_state: 'cooldown', status: 'pending' }, 'cooldown'],
    ['status next', { status: 'pending' }, 'pending'],
    ['empty strings fall through', { lifecycle_state: '', status: '' }, 'ready'],
    ['null fields', { lifecycle_state: null, status: null }, 'ready'],
    ['no fields', {}, 'ready'],
    ['unknown value is kept', { lifecycle_state: 'parked' }, 'parked'],
  ])('%s', (_, row, want) => {
    expect(lifecycleStatus(row)).toBe(want)
  })
})

describe('rollbackClassLabel', () => {
  it.each([
    ['no_rollback_needed', 'Rollback: not needed'],
    ['reversible', 'Rollback: reversible'],
    ['forward_fix_only', 'Rollback: forward fix only'],
    ['custom_class', 'Rollback: custom_class'],
    ['', null],
    [null, null],
    [undefined, null],
  ])('%s', (value, want) => {
    expect(rollbackClassLabel(value)).toBe(want)
  })
})

describe('formatActionTime', () => {
  it('formats a valid timestamp with its zone (ux-05d)', () => {
    const value = '2026-10-03T11:00:00Z'
    const got = formatActionTime(value)
    expect(got).toBe(formatTime(value))
    expect(got).toMatch(/^2026-10-0[23] \d{2}:00 \S+$/)
  })

  it.each([
    ['null', null],
    ['undefined', undefined],
    ['empty string', ''],
    ['not a date', 'yesterday-ish'],
  ])('returns null for %s', (_, value) => {
    expect(formatActionTime(value)).toBeNull()
  })

  it('treats the epoch (numeric zero) as missing, as before the split', () => {
    expect(formatActionTime(0)).toBeNull()
  })

  it('accepts a numeric epoch other than zero', () => {
    expect(formatActionTime(1000)).toBe(formatTime(1000))
    expect(formatActionTime(1000)).toMatch(/^19(69|70)-/)
  })
})

describe('actionRowKey', () => {
  it('prefers the ledger key, so queued and executed ids never collide', () => {
    const queued = actionRowKey({ database_name: 'db', ledger_key: 'queue:7', id: '7' })
    const executed = actionRowKey({ database_name: 'db', ledger_key: 'log:7', id: '7' })
    expect(queued).toBe('["db","queue:7"]')
    expect(executed).toBe('["db","log:7"]')
  })

  it('falls back to the id and an empty database', () => {
    expect(actionRowKey({ id: 41 })).toBe('["",41]')
  })

  it('keeps the same id in two databases apart', () => {
    expect(actionRowKey({ database_name: 'a', id: 1 }))
      .not.toBe(actionRowKey({ database_name: 'b', id: 1 }))
  })
})

describe('actionCaseScope', () => {
  afterEach(() => { window.location.hash = '' })

  it('reads the case from the hash query', () => {
    window.location.hash = '#/actions?database=orders&case=17'
    expect(actionCaseScope()).toBe('17')
  })

  it.each([
    ['no hash', ''],
    ['no query', '#/actions'],
    ['empty case', '#/actions?case='],
    ['other params only', '#/actions?database=orders'],
  ])('is empty with %s', (_, hash) => {
    window.location.hash = hash
    expect(actionCaseScope()).toBe('')
  })
})
