import { describe, expect, it } from 'vitest'
import {
  OWNER_NAME_MAX, instanceOwnerLabel, instanceOwnerText,
} from './instanceOwner'

// advisors-08b: which database of the fleet owns this database's instance
// settings (status.capabilities.instance_owner, advisors-08).
// No error propagation tests: the functions read one field of a response
// that has already arrived; fetch failures stay with QueryState.
// No concurrent access tests: pure functions over their argument.

const caps = instance_owner => ({ instance_owner })
const name = n => 'd'.repeat(n)

describe('instanceOwnerText', () => {
  it('names the owner on the owning database', () => {
    expect(instanceOwnerText(caps({
      owner: 'primary', is_owner: true, key_source: 'system_identifier',
    }))).toBe('Owns the instance settings')
  })

  it('names the owner on a deferring database', () => {
    expect(instanceOwnerText(caps({
      owner: 'orders', is_owner: false, key_source: 'address',
    }))).toBe('Instance settings handled by orders')
  })

  // A malformed is_owner is neither owner nor deferral: fail to "unknown".
  it('counts only a boolean true as owner', () => {
    expect(instanceOwnerText(caps({ owner: 'orders', is_owner: 'true' })))
      .toBe('Instance owner unknown')
    expect(instanceOwnerText(caps({ owner: 'orders' })))
      .toBe('Instance owner unknown')
    expect(instanceOwnerText(caps({ is_owner: 'true' })))
      .toBe('Instance owner unknown')
    expect(instanceOwnerText(caps({ is_owner: 1 })))
      .toBe('Instance owner unknown')
  })

  it('reports an unknown owner for a non-string or empty owner', () => {
    expect(instanceOwnerText(caps({ owner: 42, is_owner: false })))
      .toBe('Instance owner unknown')
    expect(instanceOwnerText(caps({ owner: '', is_owner: false })))
      .toBe('Instance owner unknown')
    expect(instanceOwnerText(caps({ owner: '   ', is_owner: false })))
      .toBe('Instance owner unknown')
    expect(instanceOwnerText(caps({ owner: ['a'], is_owner: false })))
      .toBe('Instance owner unknown')
  })

  it('reports an unknown owner for a non-object instance_owner', () => {
    expect(instanceOwnerText(caps('primary'))).toBe('Instance owner unknown')
    expect(instanceOwnerText(caps(['primary']))).toBe('Instance owner unknown')
    expect(instanceOwnerText(caps(true))).toBe('Instance owner unknown')
  })

  it('returns null when there is no instance owner to show', () => {
    expect(instanceOwnerText(undefined)).toBeNull()
    expect(instanceOwnerText(null)).toBeNull()
    expect(instanceOwnerText({})).toBeNull()
    expect(instanceOwnerText(caps(undefined))).toBeNull()
    expect(instanceOwnerText(caps(null))).toBeNull()
    expect(instanceOwnerText(caps({}))).toBeNull()
  })

  it('keeps the full owner name in the text', () => {
    expect(instanceOwnerText(caps({ owner: name(65), is_owner: false })))
      .toBe(`Instance settings handled by ${name(65)}`)
  })
})

describe('instanceOwnerLabel', () => {
  it('returns null when there is no instance owner to show', () => {
    expect(instanceOwnerLabel(undefined)).toBeNull()
    expect(instanceOwnerLabel(caps(null))).toBeNull()
    expect(instanceOwnerLabel(caps({}))).toBeNull()
  })

  it('shows a 64-character owner in full with no title', () => {
    expect(OWNER_NAME_MAX).toBe(64)
    expect(instanceOwnerLabel(caps({ owner: name(64), is_owner: false })))
      .toEqual({ text: `Instance settings handled by ${name(64)}`, title: undefined })
  })

  it('truncates a 65-character owner and keeps the full name in the title', () => {
    const label = instanceOwnerLabel(caps({ owner: name(65), is_owner: false }))
    expect(label.text).toBe(`Instance settings handled by ${name(63)}\u2026`)
    expect(label.title).toBe(name(65))
  })

  // A UTF-16 slice would cut a surrogate pair and leave a lone half.
  it('counts and cuts an owner name by characters, not UTF-16 units', () => {
    const fits = '\u{1F418}'.repeat(64)
    expect(instanceOwnerLabel(caps({ owner: fits, is_owner: false })))
      .toEqual({ text: `Instance settings handled by ${fits}`, title: undefined })
    const long = '\u{1F418}'.repeat(65)
    const label = instanceOwnerLabel(caps({ owner: long, is_owner: false }))
    expect(label.text)
      .toBe(`Instance settings handled by ${'\u{1F418}'.repeat(63)}\u2026`)
    expect(label.title).toBe(long)
  })

  it('puts the reason in the title when the owner is unknown', () => {
    expect(instanceOwnerLabel(caps({
      owner: 42, is_owner: false, reason: 'instance identity unreadable',
    }))).toEqual({
      text: 'Instance owner unknown', title: 'instance identity unreadable',
    })
    expect(instanceOwnerLabel(caps({ owner: 42 })))
      .toEqual({ text: 'Instance owner unknown', title: undefined })
  })

  it('keeps the reason in the title of a database advised on its own', () => {
    expect(instanceOwnerLabel(caps({
      is_owner: true, reason: 'instance identity unreadable',
    }))).toEqual({
      text: 'Owns the instance settings', title: 'instance identity unreadable',
    })
  })

  it('ignores a non-string reason', () => {
    expect(instanceOwnerLabel(caps({ is_owner: 'yes', reason: { x: 1 } })))
      .toEqual({ text: 'Instance owner unknown', title: undefined })
  })
})
