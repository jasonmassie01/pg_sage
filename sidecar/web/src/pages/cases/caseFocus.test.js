import { describe, expect, it } from 'vitest'
import {
  focusDatabase, focusFromHash, focusedInvestigation, investigationCase,
  investigationDetailURL, investigationID, resolveFocus,
} from './caseFocus'
import { actionFocusID, isFocusedAction } from '../actions/actionFocus'

// mcp-05: the pure focus helpers behind citation deep links.
// cases-12: a cited investigation resolves through its own record, read
// by id, never through the database's newest 100.
// No concurrency tests: pure functions of their arguments.

const UUID = '3f2a9c1e-7b4d-4e8a-9c2f-1a2b3c4d5e6f'

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
    expect(investigationCase({ kind: 'investigation', id: UUID, database: '' }, null))
      .toBeNull()
    expect(investigationCase(null, { id: UUID, case_id: 'finding:orders:a' })).toBeNull()
  })

  it('never borrows another database investigation', () => {
    const inv = { id: UUID, case_id: 'finding:billing:a', database: 'billing' }
    expect(investigationCase({ kind: 'investigation', id: UUID, database: 'orders' }, inv))
      .toBeNull()
    expect(investigationCase({ kind: 'investigation', id: UUID, database: 'billing' }, inv))
      .toBe('finding:billing:a')
  })
})

describe('investigationCase', () => {
  it('returns only a case id the cases API can focus', () => {
    const focus = { kind: 'investigation', id: UUID, database: '' }
    for (const caseID of ['ask:0123456789abcdef', '', 'nokind', 'finding:', null,
      undefined]) {
      expect(investigationCase(focus, { id: UUID, case_id: caseID })).toBeNull()
    }
    for (const caseID of ['finding:orders:a', 'incident:orders:b', 'query_hint:orders:7']) {
      expect(investigationCase(focus, { id: UUID, case_id: caseID })).toBe(caseID)
    }
  })
})

describe('investigationID', () => {
  it('accepts a canonical UUID and lower-cases an upper-case one', () => {
    expect(investigationID(UUID)).toBe(UUID)
    expect(investigationID(UUID.toUpperCase())).toBe(UUID)
  })

  it.each([['77'], ['..%2F..%2Fx'], ['../../x'], [''], [null], [undefined], [42],
    [`${UUID} `], [`${UUID}0`], [UUID.slice(1)], [UUID.replace(/-/g, '')],
    ['3f2a9c1e-7b4d-4e8a-9c2f-1a2b3c4d5e6g']])('rejects %j', id => {
    expect(investigationID(id)).toBe('')
  })
})

describe('focusDatabase', () => {
  const focus = db => ({ kind: 'investigation', id: UUID, database: db })
  it('prefers the focus database, then the selected one, never all', () => {
    expect(focusDatabase(focus('orders'), 'billing')).toBe('orders')
    expect(focusDatabase(focus(''), 'billing')).toBe('billing')
    expect(focusDatabase(focus(''), 'all')).toBe('')
    expect(focusDatabase(focus(''), '')).toBe('')
    expect(focusDatabase(focus(''), undefined)).toBe('')
    expect(focusDatabase(null, 'billing')).toBe('billing')
  })
})

describe('investigationDetailURL', () => {
  const focus = (id, db = '') => ({ kind: 'investigation', id, database: db })
  it('names the per-id route of the focused database', () => {
    expect(investigationDetailURL(focus(UUID, 'orders'), 'all'))
      .toBe(`/api/v1/databases/orders/investigations/${UUID}`)
    expect(investigationDetailURL(focus(UUID), 'billing'))
      .toBe(`/api/v1/databases/billing/investigations/${UUID}`)
  })

  it('lower-cases the id and encodes the database into one segment', () => {
    expect(investigationDetailURL(focus(UUID.toUpperCase(), 'a/b?c#d'), 'all'))
      .toBe(`/api/v1/databases/a%2Fb%3Fc%23d/investigations/${UUID}`)
  })

  it('has no url for a bad id, no database, or another focus kind', () => {
    expect(investigationDetailURL(focus('77', 'orders'), 'orders')).toBeNull()
    expect(investigationDetailURL(focus('..%2F..%2Fx', 'orders'), 'orders')).toBeNull()
    expect(investigationDetailURL(focus(UUID), 'all')).toBeNull()
    expect(investigationDetailURL(focus(UUID), '')).toBeNull()
    expect(investigationDetailURL({ kind: 'finding', id: UUID, database: 'o' }, 'o'))
      .toBeNull()
    expect(investigationDetailURL(null, 'orders')).toBeNull()
  })
})

describe('focusedInvestigation', () => {
  const focus = { kind: 'investigation', id: UUID.toUpperCase(), database: 'orders' }
  it('is the investigation when its id and database match the focus', () => {
    const inv = { id: UUID, case_id: 'finding:orders:a', database: 'orders' }
    expect(focusedInvestigation(focus, inv)).toBe(inv)
    expect(focusedInvestigation({ ...focus, database: '' }, inv)).toBe(inv)
  })

  it('resolves the focus to the case the investigation names', () => {
    const inv = { id: UUID, case_id: 'incident:orders:lock:1', database: 'orders' }
    expect(resolveFocus(focus, [], inv)).toBe('incident:orders:lock:1')
    expect(resolveFocus(focus, [], { ...inv, case_id: 'ask:0123456789abcdef' })).toBeNull()
    expect(resolveFocus(focus, [], null)).toBeNull()
  })

  it('is null for another id, another database, no record or no focus', () => {
    const other = '00000000-0000-4000-8000-000000000000'
    expect(focusedInvestigation(focus, { id: other, database: 'orders' })).toBeNull()
    expect(focusedInvestigation(focus, { id: UUID, database: 'billing' })).toBeNull()
    expect(focusedInvestigation(focus, null)).toBeNull()
    expect(focusedInvestigation(focus, undefined)).toBeNull()
    expect(focusedInvestigation(null, { id: UUID })).toBeNull()
    expect(focusedInvestigation({ ...focus, kind: 'case' }, { id: UUID })).toBeNull()
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
