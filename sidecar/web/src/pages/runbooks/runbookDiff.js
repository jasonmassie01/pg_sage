// A structural diff of two runbook definitions (playbooks-10), shown to an
// admin before signing. Nodes are matched by id: a node only in the base
// is removed, a node only in the new definition is added, and a node in
// both lists the fields whose values differ. Top-level fields (name,
// trigger, start, ...) are compared as whole values. Key order does not
// matter, and an empty value ('', [], {}, null) equals an absent one, as
// in the stored JSON. There is no text diff.

function isEmpty(v) {
  if (v === undefined || v === null || v === '') return true
  if (Array.isArray(v)) return v.length === 0
  return typeof v === 'object' && Object.keys(v).length === 0
}

// canonical renders a value with sorted object keys, so equal values
// compare equal whatever their key order.
function canonical(v) {
  if (Array.isArray(v)) return `[${v.map(canonical).join(',')}]`
  if (v && typeof v === 'object') {
    const keys = Object.keys(v).filter(k => !isEmpty(v[k])).sort()
    return `{${keys.map(k => `${JSON.stringify(k)}:${canonical(v[k])}`).join(',')}}`
  }
  return JSON.stringify(v)
}

function same(a, b) {
  if (isEmpty(a) && isEmpty(b)) return true
  return canonical(a) === canonical(b)
}

function changedFields(a, b, skip) {
  const keys = new Set([...Object.keys(a || {}), ...Object.keys(b || {})])
  return [...keys].filter(k => k !== skip && !same(a?.[k], b?.[k])).sort()
}

function nodesOf(def) {
  const nodes = Array.isArray(def?.nodes) ? def.nodes : []
  return nodes.filter(n => n && typeof n === 'object' && typeof n.id === 'string')
}

// diffDefinitions lists the changes from `from` (the newest signed
// version, or nothing) to `to`: top-level fields first, then removed,
// added and changed nodes.
export function diffDefinitions(from, to) {
  const top = changedFields(from, to, 'nodes').map(field => ({ kind: 'changed', field }))
  const before = new Map(nodesOf(from).map(n => [n.id, n]))
  const after = new Map(nodesOf(to).map(n => [n.id, n]))
  const removed = [...before.keys()].filter(id => !after.has(id))
    .map(node => ({ kind: 'removed', node }))
  const added = [...after.keys()].filter(id => !before.has(id))
    .map(node => ({ kind: 'added', node }))
  const changed = [...after.keys()].filter(id => before.has(id))
    .map(node => ({ kind: 'changed', node, fields: changedFields(before.get(node),
      after.get(node), 'id') }))
    .filter(c => c.fields.length > 0)
  return [...top, ...removed, ...added, ...changed]
}

// describeChange renders one change for the sign dialog.
export function describeChange(c) {
  if (!c.node) return `changed ${c.field}`
  if (c.kind !== 'changed') return `${c.kind} node ${c.node}`
  return `changed node ${c.node}: ${c.fields.join(', ')}`
}
