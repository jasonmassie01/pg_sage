import { describe, expect, it } from 'vitest'
import {
  autoFamilies, fleetAutonomy, fleetAutonomyHint, fleetBadge, rowAutoFamilies,
  unreportedNote,
} from './autonomy'

// dashboard-02: what pg_sage may run unattended right now, per database and
// across the fleet. Pure functions over the fleet poll.
// No concurrent access tests: stateless functions over their arguments.

const fam = (action_type, decision) => ({ action_type, decision })
const row = (name, trust_level, families, extra = {}) => ({
  name,
  emergency_stopped: false,
  status: {
    trust_level,
    capabilities: families === undefined ? {} : { action_families: families },
  },
  ...extra,
})

describe('autoFamilies', () => {
  it('lists only the families whose decision is execute, in order', () => {
    expect(autoFamilies({ action_families: [
      fam('create_index', 'execute'), fam('vacuum_table', 'queue_for_approval'),
      fam('analyze_table', 'execute'), fam('drop_index', 'blocked'),
    ] })).toEqual(['create_index', 'analyze_table'])
  })

  it('returns undefined when the policy was not reported', () => {
    expect(autoFamilies({})).toBeUndefined()
    expect(autoFamilies(null)).toBeUndefined()
    expect(autoFamilies(undefined)).toBeUndefined()
    expect(autoFamilies({ action_families: 'execute' })).toBeUndefined()
  })

  it('returns an empty list when nothing executes', () => {
    expect(autoFamilies({ action_families: [] })).toEqual([])
    expect(autoFamilies({ action_families: [fam('analyze_table', 'queue')] }))
      .toEqual([])
  })

  it('drops malformed entries instead of naming them', () => {
    expect(autoFamilies({ action_families: [
      null, { decision: 'execute' }, fam('', 'execute'), fam(7, 'execute'),
      fam('analyze_table', 'EXECUTE'), fam('reindex', 'execute'),
    ] })).toEqual(['reindex'])
  })
})

describe('rowAutoFamilies', () => {
  it('reports nothing for an emergency-stopped database, even with stale data', () => {
    const stopped = row('a', 'autonomous', [fam('create_index', 'execute')],
      { emergency_stopped: true })
    expect(rowAutoFamilies(stopped)).toEqual([])
  })

  it('reads the selected row', () => {
    expect(rowAutoFamilies(row('a', 'advisory', [fam('analyze_table', 'execute')])))
      .toEqual(['analyze_table'])
    expect(rowAutoFamilies(row('a', 'advisory'))).toBeUndefined()
    expect(rowAutoFamilies(null)).toBeUndefined()
  })
})

describe('fleetAutonomy', () => {
  it('counts levels and names the databases that run each family', () => {
    const got = fleetAutonomy([
      row('db2', 'autonomous', [fam('create_index', 'execute'), fam('analyze_table', 'execute')]),
      row('db1', 'autonomous', [fam('create_index', 'execute')]),
      row('db3', 'observation', [fam('create_index', 'blocked')]),
    ])
    expect(got.levels).toEqual({ observation: 1, advisory: 0, autonomous: 2 })
    expect(got.unknown).toBe(0)
    expect(got.autoByFamily).toEqual({
      analyze_table: ['db2'], create_index: ['db1', 'db2'],
    })
    expect(got.reported).toBe(3)
    expect(got.total).toBe(3)
  })

  it('counts an unknown or missing trust level as unknown, never autonomous', () => {
    const got = fleetAutonomy([
      row('a', 'superuser', [fam('create_index', 'execute')]),
      row('b', null),
      { name: 'c' },
      row('d', 'Autonomous'),
    ])
    expect(got.levels.autonomous).toBe(1)
    expect(got.unknown).toBe(3)
    expect(got.total).toBe(4)
  })

  it('leaves an emergency-stopped database out of every family', () => {
    const got = fleetAutonomy([
      row('a', 'autonomous', [fam('create_index', 'execute')], { emergency_stopped: true }),
      row('b', 'autonomous', [fam('create_index', 'execute')]),
    ])
    expect(got.autoByFamily).toEqual({ create_index: ['b'] })
  })

  it('handles an empty or missing fleet', () => {
    for (const rows of [[], null, undefined]) {
      expect(fleetAutonomy(rows)).toEqual({
        levels: { observation: 0, advisory: 0, autonomous: 0 },
        autoByFamily: {}, unknown: 0, reported: 0, total: 0,
      })
    }
  })
})

describe('fleetAutonomyHint', () => {
  it('names each family with the databases that run it', () => {
    const hint = fleetAutonomyHint(fleetAutonomy([
      row('db1', 'autonomous', [fam('create_index', 'execute')]),
      row('db2', 'autonomous', [fam('create_index', 'execute')]),
      row('db3', 'observation', []),
    ]))
    expect(hint).toBe('Auto-executes now: create_index on db1, db2')
  })

  it('says nothing auto-executes when every database reported none', () => {
    expect(fleetAutonomyHint(fleetAutonomy([row('a', 'advisory', [])])))
      .toMatch(/^Nothing auto-executes right now/)
  })

  it('returns null when no database reported its policy', () => {
    expect(fleetAutonomyHint(fleetAutonomy([row('a', 'advisory')]))).toBeNull()
    expect(fleetAutonomyHint(fleetAutonomy([]))).toBeNull()
  })

  it('says how many databases did not report', () => {
    const hint = fleetAutonomyHint(fleetAutonomy([
      row('a', 'advisory', []), row('b', 'advisory'),
    ]))
    expect(hint).toMatch(/Nothing auto-executes right now/)
    expect(hint).toMatch(/1 database has not reported/)
  })
})

describe('fleetBadge', () => {
  it('summarises a mixed fleet, most autonomous first', () => {
    const badge = fleetBadge(fleetAutonomy([
      row('a', 'autonomous'), row('b', 'observation'), row('c', 'autonomous'),
    ]))
    expect(badge).toEqual({ kind: 'mixed', label: 'Mixed · 2 autonomous · 1 observation' })
  })

  it('renders a one-level fleet as that level', () => {
    expect(fleetBadge(fleetAutonomy([row('a', 'observation'), row('b', 'observation')])))
      .toEqual({ kind: 'single', level: 'observation' })
  })

  it('treats an unknown database as a different level', () => {
    expect(fleetBadge(fleetAutonomy([row('a', 'autonomous'), row('b', 'bogus')])))
      .toEqual({ kind: 'mixed', label: 'Mixed · 1 autonomous · 1 unknown' })
  })

  it('shows no badge when no level is known', () => {
    expect(fleetBadge(fleetAutonomy([row('a', null)]))).toBeNull()
    expect(fleetBadge(fleetAutonomy([]))).toBeNull()
  })
})

describe('unreportedNote', () => {
  it('names one database that did not report, in the singular', () => {
    expect(unreportedNote(fleetAutonomy([row('a', 'advisory', []), row('b', 'advisory')])))
      .toBe('1 database has not reported their action policy')
  })

  it('names several databases that did not report, in the plural', () => {
    expect(unreportedNote(fleetAutonomy([
      row('a', 'autonomous', [fam('create_index', 'execute')]),
      row('b', 'advisory'), row('c', null),
    ]))).toBe('2 databases have not reported their action policy')
  })

  it('is null when every database reported', () => {
    expect(unreportedNote(fleetAutonomy([row('a', 'advisory', []), row('b', 'observation', [])])))
      .toBeNull()
  })

  it('is null when no database reported, since nothing was claimed', () => {
    expect(unreportedNote(fleetAutonomy([row('a', 'advisory'), row('b', 'advisory')]))).toBeNull()
    expect(unreportedNote(fleetAutonomy([]))).toBeNull()
    expect(unreportedNote(undefined)).toBeNull()
    expect(unreportedNote(null)).toBeNull()
  })

  it('counts an emergency-stopped database without families as reported', () => {
    const stopped = row('b', 'autonomous', undefined, { emergency_stopped: true })
    expect(unreportedNote(fleetAutonomy([row('a', 'advisory', []), stopped]))).toBeNull()
  })
})
