import { describe, expect, it } from 'vitest'
import { describeChange, diffDefinitions } from './runbookDiff'

// A structural diff of two runbook definitions by node id (playbooks-10):
// what an admin reviews before signing. Nodes are matched by id; a changed
// node lists its changed fields; top-level fields are compared as values.

function def() {
  return {
    name: 'Idle holder', description: '',
    trigger: { kinds: ['lock_blocking'], nodes: ['idle_in_tx_holder'] },
    start: 'read_chains',
    nodes: [
      { id: 'read_chains', type: 'probe', probe: 'lock_chains', next: 'is_idle',
        args: { limit: 5 } },
      { id: 'is_idle', type: 'decision', when: { op: 'hypothesis',
        node: 'idle_in_tx_holder', in: ['root_cause'] }, then: 'end_tx', else: 'esc' },
      { id: 'end_tx', type: 'proposal', proposal: { kind: 'operator_step',
        node: 'idle_in_tx_holder' } },
      { id: 'esc', type: 'proposal', proposal: { kind: 'escalate' } },
    ],
  }
}

describe('diffDefinitions', () => {
  it('gives no changes for identical definitions', () => {
    expect(diffDefinitions(def(), def())).toEqual([])
  })

  it('ignores key order inside nodes and nested values', () => {
    const b = def()
    b.nodes[1] = { else: 'esc', then: 'end_tx', type: 'decision', id: 'is_idle',
      when: { in: ['root_cause'], node: 'idle_in_tx_holder', op: 'hypothesis' } }
    expect(diffDefinitions(def(), b)).toEqual([])
  })

  it('treats a renamed node as one removal plus one addition', () => {
    const b = def()
    b.nodes[3] = { ...b.nodes[3], id: 'escalate_now' }
    const changes = diffDefinitions(def(), b).filter(c => c.node !== 'is_idle')
    expect(changes).toEqual([
      { kind: 'removed', node: 'esc' },
      { kind: 'added', node: 'escalate_now' },
    ])
  })

  it('reports a renamed node and the edge that now points at it', () => {
    const b = def()
    b.nodes[3] = { ...b.nodes[3], id: 'escalate_now' }
    b.nodes[1] = { ...b.nodes[1], else: 'escalate_now' }
    expect(diffDefinitions(def(), b)).toEqual([
      { kind: 'removed', node: 'esc' },
      { kind: 'added', node: 'escalate_now' },
      { kind: 'changed', node: 'is_idle', fields: ['else'] },
    ])
  })

  it('reports a changed probe argument as one change', () => {
    const b = def()
    b.nodes[0] = { ...b.nodes[0], args: { limit: 10 } }
    expect(diffDefinitions(def(), b)).toEqual([
      { kind: 'changed', node: 'read_chains', fields: ['args'] },
    ])
  })

  it('reports changed top-level fields and an added field; empty equals absent', () => {
    const b = def()
    b.trigger = { kinds: ['lock_blocking'] }
    b.start = 'is_idle'
    delete b.description
    b.nodes[2] = { ...b.nodes[2], proposal: { kind: 'operator_step',
      node: 'idle_in_tx_holder' }, note: 'x' }
    expect(diffDefinitions(def(), b)).toEqual([
      { kind: 'changed', field: 'start' },
      { kind: 'changed', field: 'trigger' },
      { kind: 'changed', node: 'end_tx', fields: ['note'] },
    ])
  })

  it('treats a missing base as every node added', () => {
    const changes = diffDefinitions(null, def())
    expect(changes.filter(c => c.kind === 'added').map(c => c.node))
      .toEqual(['read_chains', 'is_idle', 'end_tx', 'esc'])
    expect(changes.filter(c => c.kind === 'removed')).toEqual([])
  })

  it('tolerates empty or malformed definitions', () => {
    expect(diffDefinitions({}, {})).toEqual([])
    expect(diffDefinitions(undefined, undefined)).toEqual([])
    expect(diffDefinitions({ nodes: 'x' }, { nodes: [null, { type: 'probe' }] })).toEqual([])
  })
})

describe('describeChange', () => {
  it('names the node and fields of each change', () => {
    expect(describeChange({ kind: 'added', node: 'a' })).toBe('added node a')
    expect(describeChange({ kind: 'removed', node: 'b' })).toBe('removed node b')
    expect(describeChange({ kind: 'changed', node: 'c', fields: ['args', 'next'] }))
      .toBe('changed node c: args, next')
    expect(describeChange({ kind: 'changed', field: 'trigger' })).toBe('changed trigger')
  })
})
