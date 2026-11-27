import { useCallback, useState } from 'react'
import { parseTagAssignment, tagUpdateBody } from './fleetList'

// useBulkTag sets one key=value tag on each selected registration (ux-10a),
// one at a time, through the per-row routes: GET the stored record, then
// PUT it back with the tag added, so no other field (nor the password)
// changes. onChanged reloads the registrations when any row changed.

async function managedJSON(url, init, what) {
  const res = await fetch(url, { credentials: 'include', ...init })
  let body = null
  try {
    body = await res.json()
  } catch (err) {
    if (!(err instanceof SyntaxError)) throw err
  }
  if (!res.ok) throw new Error(`${what}: ${body?.error || `HTTP ${res.status}`}`)
  return body
}

async function tagOne(db, tag) {
  const url = `/api/v1/databases/managed/${db.id}`
  const rec = await managedJSON(url, {}, `Reading ${db.name} failed`)
  await managedJSON(url, {
    method: 'PUT', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(tagUpdateBody(rec, tag)),
  }, `Tagging ${db.name} failed`)
}

export function useBulkTag(onError, onChanged) {
  const [tagging, setTagging] = useState(false)
  const [summary, setSummary] = useState('')
  const run = useCallback(async (dbs, raw) => {
    const tag = parseTagAssignment(raw)
    if (!tag) {
      setSummary('Type a tag as key=value')
      return false
    }
    setTagging(true)
    setSummary('')
    let ok = 0
    for (const db of dbs) {
      try {
        await tagOne(db, tag)
        ok += 1
      } catch (err) {
        onError(err.message)
      }
    }
    setTagging(false)
    setSummary(`Tagged ${dbs.length} with ${tag.key}=${tag.value}: ${ok} updated, `
      + `${dbs.length - ok} failed`)
    if (ok > 0) onChanged()
    return true
  }, [onError, onChanged])
  return { tagging, summary, run }
}
