// fleetView: the search, tag filters and sort of a fleet table (ux-10a),
// the server page URL they make, and the row selection a change of view
// clears. The managed table and the read-only one share it.
import { useCallback, useEffect, useState } from 'react'
import { DEFAULT_SORT, fleetListURL } from './fleetList'

const SEARCH_DELAY_MS = 250

function useDebounced(value, delay) {
  const [settled, setSettled] = useState(value)
  useEffect(() => {
    const id = setTimeout(() => setSettled(value), delay)
    return () => clearTimeout(id)
  }, [value, delay])
  return settled
}

export function useFleetView() {
  const [query, setQuery] = useState('')
  const [tags, setTags] = useState([])
  const [sort, setSort] = useState(DEFAULT_SORT)
  const [selected, setSelected] = useState(() => new Set())
  const q = useDebounced(query, SEARCH_DELAY_MS)
  const url = fleetListURL({ q, tags, sort })
  const [viewURL, setViewURL] = useState(url)
  if (viewURL !== url) {
    setViewURL(url)
    setSelected(new Set())
  }
  const filtered = q.trim() !== '' || tags.length > 0
  function clear() {
    setQuery('')
    setTags([])
  }
  const toggle = useCallback(id => setSelected(prev => {
    const next = new Set(prev)
    if (next.has(id)) next.delete(id)
    else next.add(id)
    return next
  }), [])
  return { query, setQuery, tags, setTags, sort, setSort, selected, setSelected,
    url, filtered, clear, toggle }
}
