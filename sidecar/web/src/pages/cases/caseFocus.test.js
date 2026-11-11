import { describe, expect, it } from 'vitest'
import { focusFromHash, investigationCase, resolveFocus } from './caseFocus'
import { actionFocusID, isFocusedAction } from '../actions/actionFocus'

// mcp-05: the pure focus helpers behind citation deep links.
// No concurrency tests: pure functions of their arguments.

describe('focusFromHash', () => {
  it('reads a finding, investigation or case focus with its database', () => {
    expect(focusFromHash('#/cases?finding=42&database=orders'))
      .toEqual({ kind: 'finding', id: '42', database: 'orders' })
    expect(focusFromHash('#/cases?investigation=9b2e'))
      .toEqual({ kind: 'investigation', id: '9b2e', database: '' })
  })

  it('prefers an explicit case over a finding', () => {
    expect(focusFromHash('#/cases?finding=1&case=c-1').kind).toBe('case')
  })

  it.each([[''], [null], [undefined], ['#/actions?finding=1'], ['#/cases'],
    ['#/cases?finding='], ['#/cases?database=orders']])('has no focus for %j', hash => {
    expect(focusFromHash(hash)).toBeNull()
  })
})

describe('resolveFocus', () => {
  const cases = [
    { id: 'c-1', source_type: 'schema_health', source_ids: [42], database_name: 'orders' },
    { id: 'c-2', source_type: 'incident', source_ids: ['43'], database_name: 'orders' },
  ]

  it('matches a numeric source id as a string and only finding-backed cases', () => {
    expect(resolveFocus({ kind: 'finding', id: '42', database: 'orders' }, cases, []))
      .toBe('c-1')
    expect(resolveFocus({ kind: 'finding', id: '43', database: 'orders' }, cases, []))
      .toBeNull()
  })

  it('tolerates nil cases, investigations and focus', () => {
    expect(resolveFocus(null, cases, [])).toBeNull()
    expect(resolveFocus({ kind: 'finding', id: '42', database: '' }, null, null)).toBeNull()
    expect(investigationCase({ kind: 'investigation', id: 'x', database: '' }, null))
      .toBeNull()
    expect(investigationCase(null, [{ id: 'x', case_id: 'c-1' }])).toBeNull()
  })

  it('never borrows another database investigation', () => {
    const invs = [{ id: 'x', case_id: 'finding:billing:a', database: 'billing' }]
    expect(investigationCase({ kind: 'investigation', id: 'x', database: 'orders' }, invs))
      .toBeNull()
    expect(investigationCase({ kind: 'investigation', id: 'x', database: 'billing' }, invs))
      .toBe('finding:billing:a')
  })
})

describe('investigationCase', () => {
  it('returns only a case id the cases API can focus', () => {
    const focus = { kind: 'investigation', id: 'x', database: '' }
    for (const caseID of ['ask:0123456789abcdef', '', 'nokind', 'finding:', null]) {
      expect(investigationCase(focus, [{ id: 'x', case_id: caseID }])).toBeNull()
    }
    for (const caseID of ['finding:orders:a', 'incident:orders:b', 'query_hint:orders:7']) {
      expect(investigationCase(focus, [{ id: 'x', case_id: caseID }])).toBe(caseID)
    }
  })
})

describe('actionFocus', () => {
  it('reads the action id and matches only the executed record', () => {
    expect(actionFocusID('#/actions?action=7&database=orders')).toBe('7')
    expect(actionFocusID('#/actions')).toBe('')
    expect(actionFocusID(null)).toBe('')
    expect(isFocusedAction({ id: 7, record_kind: 'executed' }, '7')).toBe(true)
    expect(isFocusedAction({ id: 7, record_kind: 'queued' }, '7')).toBe(false)
    expect(isFocusedAction({ id: 7 }, '')).toBe(false)
    expect(isFocusedAction(null, '7')).toBe(false)
  })
})
