// useAskRun.js — one asynchronous Ask Sage run (mcp-07b). The page starts
// a run, then polls it every second (every 5 s once it has run 30 s) until
// it ends; closing the tab does not stop it, and a reload resumes polling
// from the ?run= parameter of the page's hash route. Cancel posts once.

import { useCallback, useEffect, useRef, useState } from 'react'
import { cancelRunURL, runURL } from '../../lib/ask'

export const POLL_FAST_MS = 1000
export const POLL_SLOW_MS = 5000
export const BACKOFF_AFTER_MS = 30000

const TERMINAL = new Set(['done', 'cancelled', 'interrupted', 'failed'])

// pollDelay is the wait before the next poll of a run started elapsedMs ago.
export function pollDelay(elapsedMs) {
  return elapsedMs >= BACKOFF_AFTER_MS ? POLL_SLOW_MS : POLL_FAST_MS
}

function hashParts() {
  const hash = window.location.hash || ''
  const at = hash.indexOf('?')
  const path = at >= 0 ? hash.slice(0, at) : hash
  return { path, params: new URLSearchParams(at >= 0 ? hash.slice(at + 1) : '') }
}

// readRunParam is the run id in the page's hash route, or ''.
export function readRunParam() {
  return hashParts().params.get('run') || ''
}

// writeRunParam sets (or with '' removes) ?run= without a navigation.
export function writeRunParam(id) {
  const { path, params } = hashParts()
  if (id) params.set('run', id)
  else params.delete('run')
  const query = params.toString()
  const hash = `${path || '#/ask'}${query ? `?${query}` : ''}`
  window.history.replaceState(window.history.state, '',
    `${window.location.pathname}${window.location.search}${hash}`)
}

// runOutcomeError is the message of a run that ended without concluding.
export function runOutcomeError(run) {
  if (run.state === 'failed') {
    return { kind: 'run', message: `The question failed: ${run.error || 'unknown error'}` }
  }
  if (run.state === 'interrupted') {
    return { kind: 'run', message: 'The question was interrupted because pg_sage restarted.' }
  }
  if (run.state === 'cancelled' && !run.answer) {
    return { kind: 'run', message: 'The question was cancelled before Sage took any action.' }
  }
  return null
}

// useAskRun tracks the active run. start(id) resolves true when the run
// ends with an answer. requestJSON is injected (useAskSession's).
export function useAskRun(database, requestJSON, { onAnswer, onError }) {
  const [run, setRun] = useState(null)
  const timer = useRef(null)
  const live = useRef(true)
  const active = useRef(null)
  const pollRef = useRef(null)
  useEffect(() => {
    live.current = true
    return () => {
      live.current = false
      clearTimeout(timer.current)
    }
  }, [])

  const finish = useCallback(data => {
    const current = active.current
    if (!current || current.id !== data.id) return
    active.current = null
    clearTimeout(timer.current)
    writeRunParam('')
    setRun(null)
    if (data.answer) onAnswer(data.answer)
    const err = runOutcomeError(data)
    if (err) onError(err)
    current.resolve(Boolean(data.answer))
  }, [onAnswer, onError])

  const poll = useCallback(async id => {
    let r
    try {
      r = await requestJSON(runURL(database, id))
    } catch (err) {
      if (!(err instanceof TypeError)) throw err
      r = null // a network blip: keep polling
    }
    if (!live.current || active.current?.id !== id) return
    if (r && !r.ok) {
      finish({ id, state: 'failed', error: r.error.message })
      return
    }
    const data = r?.data
    if (data && TERMINAL.has(data.state)) {
      finish({ ...data, id })
      return
    }
    if (data) {
      setRun({ id, state: data.state, cancelling: active.current.cancelling,
        progress: Array.isArray(data.progress) ? data.progress : [] })
    }
    const elapsed = Date.now() - active.current.startedAt
    timer.current = setTimeout(() => pollRef.current?.(id), pollDelay(elapsed))
  }, [database, requestJSON, finish])
  useEffect(() => { pollRef.current = poll }, [poll])

  const start = useCallback(id => new Promise(resolve => {
    active.current = { id, startedAt: Date.now(), resolve, cancelling: false }
    writeRunParam(id)
    setRun({ id, state: 'queued', progress: [], cancelling: false })
    poll(id)
  }), [poll])

  const cancel = useCallback(async () => {
    const current = active.current
    if (!current || current.cancelling) return
    current.cancelling = true
    setRun(r => (r ? { ...r, cancelling: true } : r))
    try {
      const r = await requestJSON(cancelRunURL(database, current.id), {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}',
      })
      if (r.ok && r.data) finish({ ...r.data, id: current.id })
    } catch (err) {
      if (!(err instanceof TypeError)) throw err
    }
  }, [database, requestJSON, finish])

  // A reload resumes the run named in the URL.
  useEffect(() => {
    const id = readRunParam()
    if (id && !active.current) start(id)
  }, [start])

  return { run, start, cancel }
}
