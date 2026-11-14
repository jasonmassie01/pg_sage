import { useCallback, useEffect, useState } from 'react'

// rules-12b: the Cases page's finding source lives in the hash
// (#/cases?finding_source=<subsystem>), so a reload keeps it.

const PARAM = 'finding_source'

function splitHash(hash) {
  const text = String(hash || '')
  const at = text.indexOf('?')
  return at < 0 ? [text, ''] : [text.slice(0, at), text.slice(at + 1)]
}

// findingSourceFromHash is the hash's finding source, or '' for All.
export function findingSourceFromHash(hash = window.location.hash) {
  return new URLSearchParams(splitHash(hash)[1]).get(PARAM) || ''
}

// hashWithFindingSource is hash with its finding source set to source,
// or removed for All (''), keeping the path and the other parameters.
export function hashWithFindingSource(hash, source) {
  const [path, query] = splitHash(hash)
  const params = new URLSearchParams(query)
  if (source) params.set(PARAM, source)
  else params.delete(PARAM)
  const next = params.toString()
  return next ? `${path}?${next}` : path
}

// useFindingSource is [source, setSource]: the hash's finding source,
// following hash changes, and a setter that writes it to the hash.
export function useFindingSource() {
  const [source, setSource] = useState(() => findingSourceFromHash())
  useEffect(() => {
    const onHashChange = () => setSource(findingSourceFromHash())
    window.addEventListener('hashchange', onHashChange)
    return () => window.removeEventListener('hashchange', onHashChange)
  }, [])
  const choose = useCallback(next => {
    const hash = hashWithFindingSource(window.location.hash, next)
    if (hash !== window.location.hash) window.location.hash = hash
    setSource(next)
  }, [])
  return [source, choose]
}
