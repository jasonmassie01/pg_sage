import { describe, expect, it } from 'vitest'
import {
  budgetState, connectionLabel, findingsLabel, formatRun, haLabel, joinRuntime,
  stopLabel, trustState,
} from './runtimeStatus'

// fleet-06 runtime wording. No concurrent access tests: pure functions.

describe('joinRuntime', () => {
  it('joins on database_id, then on name only when an id is missing', () => {
    const a = { id: 1, name: 'a' }
    const b = { id: 2, name: 'b' }
    const c = { id: 0, name: 'c' }
    const runtime = [
      { database_id: 1, name: 'a-renamed' },
      { database_id: 7, name: 'b' },
      { database_id: 0, name: 'c' },
      { database_id: 0, name: 'yaml-only' },
    ]
    const { runtimeFor, unjoined } = joinRuntime([a, b, c], runtime)
    expect(runtimeFor(a)).toBe(runtime[0])
    expect(runtimeFor(b)).toBeUndefined()
    expect(runtimeFor(c)).toBe(runtime[2])
    expect(unjoined.map(r => r.name)).toEqual(['b', 'yaml-only'])
  })

  it('handles empty inputs', () => {
    const { runtimeFor, unjoined } = joinRuntime([], [])
    expect(runtimeFor({ id: 1, name: 'x' })).toBeUndefined()
    expect(unjoined).toEqual([])
  })
})

describe('runtime labels', () => {
  it('distinguishes not running from unknown', () => {
    expect(connectionLabel(undefined, true)).toBe('not running')
    expect(connectionLabel(undefined, false)).toBe('unknown')
    expect(connectionLabel({ status: {} })).toBe('unknown')
    expect(connectionLabel({ status: { connected: false } })).toBe('disconnected')
  })

  it('names a known HA role and safe mode only', () => {
    expect(haLabel({ ha_role: 'unknown' })).toBe('')
    expect(haLabel({ ha_role: 'replica', ha_safe_mode: true })).toBe('replica · HA safe mode')
    expect(haLabel(undefined)).toBe('')
  })

  it('counts findings, defaulting a missing critical count to 0', () => {
    expect(findingsLabel({ findings_open: 0 })).toBe('0 open · 0 critical')
    expect(findingsLabel({})).toBe('—')
  })

  it('marks an override only against a registration', () => {
    expect(trustState({ trust_level: 'autonomous' }, 'advisory'))
      .toEqual({ effective: 'autonomous', override: true })
    expect(trustState({ trust_level: 'advisory' }, 'advisory').override).toBe(false)
    expect(trustState({ trust_level: 'advisory' }).override).toBe(false)
    expect(trustState({}, 'advisory')).toEqual({ effective: 'unknown', override: false })
  })

  it('reads Go zero and invalid times as never', () => {
    expect(formatRun('0001-01-01T00:00:00Z')).toBe('never')
    expect(formatRun('not a time')).toBe('never')
    expect(formatRun(null)).toBe('never')
    expect(formatRun('2026-11-10T12:00:00Z')).not.toBe('never')
  })

  it('attributes a stop, even without an actor or time', () => {
    expect(stopLabel(undefined)).toBe('—')
    expect(stopLabel({ emergency_stopped: false })).toBe('not stopped')
    expect(stopLabel({ emergency_stopped: true })).toBe('stopped by an unknown actor')
    expect(stopLabel({ emergency_stopped: true, emergency_stopped_by: 'bob' }))
      .toBe('stopped by bob')
  })

  it('reports a budget at its boundaries', () => {
    expect(budgetState({ llm_budget: { allocation: 10, used: 9 } }))
      .toEqual({ label: '9 / 10', exhausted: false })
    expect(budgetState({ llm_budget: { allocation: 10, used: 10 } }).exhausted).toBe(true)
    expect(budgetState({ llm_budget: { allocation: 0, used: 0 } }))
      .toEqual({ label: '0 / 0', exhausted: true })
    expect(budgetState({ llm_budget: { allocation: 'x', used: 1 } }).label).toBe('—')
    expect(budgetState(undefined)).toEqual({ label: '—', exhausted: false })
  })
})
