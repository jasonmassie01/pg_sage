import { useCallback, useMemo, useState } from 'react'
import { withSurface } from '../../lib/inbox'

// Bulk approve (ux-07b): the helpers behind the Waiting on you and Inbox
// selections. The server decides each item on its own (its card hash, the
// gate, safe tier only) and answers one result per item; these build the
// request, group same-class safe items and keep the refused selected.

export const BULK_APPROVE_PATH = '/api/v1/approvals/bulk-approve'
export const BULK_MAX = 50

export const isSafeTier = tier => typeof tier === 'string'
  && tier.trim().toLowerCase() === 'safe'

export const bulkKey = (database, id) => `${database}:${id}`

export const SAFE_ONLY = 'Bulk approve is safe tier only; approve this one on its card.'

// classGroups lists, per database and class, the keys of two or more safe
// entries ({key, cls, database, safe}), sorted by database then class.
export function classGroups(entries) {
  const groups = new Map()
  for (const e of entries || []) {
    if (!e.safe || !e.cls) continue
    const id = `${e.database}\u0000${e.cls}`
    if (!groups.has(id)) groups.set(id, { cls: e.cls, database: e.database, keys: [] })
    groups.get(id).keys.push(e.key)
  }
  return [...groups.values()].filter(g => g.keys.length >= 2)
    .sort((a, b) => a.database.localeCompare(b.database) || a.cls.localeCompare(b.cls))
}

// bulkItems is the request's items: each card's id, database and the hash
// the person was shown.
export const bulkItems = cards => cards.map(c => ({ id: Number(c.queue_id),
  database: c.database, card_hash: c.card_hash }))

// keepSelected drops the decided keys (approved, scheduled, failed) from
// selected and keeps the refused ones, which still wait.
export function keepSelected(selected, results) {
  const next = new Set(selected)
  for (const r of results || []) {
    if (r.status !== 'refused') next.delete(bulkKey(r.database, r.id))
  }
  return next
}

// bulkSummary names each non-zero count of a bulk answer.
export function bulkSummary(body) {
  const parts = ['approved', 'scheduled', 'refused', 'failed']
    .filter(k => Number(body?.[k]) > 0).map(k => `${body[k]} ${k}`)
  const ran = Number(body?.approved) > 0 || Number(body?.scheduled) > 0
  if (ran) return parts.join(', ')
  return parts.length ? `Nothing ran: ${parts.join(', ')}` : 'Nothing ran'
}

export async function postBulkApprove(items, surface) {
  if (!items?.length) throw new Error('Nothing selected to approve.')
  if (items.length > BULK_MAX) {
    throw new Error(`Bulk approve takes at most ${BULK_MAX} items; ${items.length} selected.`)
  }
  const res = await fetch(BULK_APPROVE_PATH, {
    method: 'POST', credentials: 'include',
    headers: withSurface({ 'Content-Type': 'application/json' }, surface),
    body: JSON.stringify({ items }),
  })
  let body = null
  try { body = await res.json() } catch { /* no JSON body: the status says it */ }
  if (!res.ok) throw new Error(body?.error || `Bulk approve failed (${res.status})`)
  return body
}

// useBulkSelection keeps the marked keys that are still in the list.
export function useBulkSelection(listKeys) {
  const [marked, setMarked] = useState(() => new Set())
  const present = useMemo(() => new Set(listKeys), [listKeys])
  const selected = useMemo(() => new Set([...marked].filter(k => present.has(k))),
    [marked, present])
  const toggle = useCallback(key => setMarked(prev => {
    const next = new Set(prev)
    if (next.has(key)) next.delete(key)
    else next.add(key)
    return next
  }), [])
  const add = useCallback(keys => setMarked(prev => new Set([...prev, ...keys])), [])
  const settle = useCallback(results => setMarked(prev => keepSelected(prev, results)), [])
  return { selected, toggle, add, settle }
}
