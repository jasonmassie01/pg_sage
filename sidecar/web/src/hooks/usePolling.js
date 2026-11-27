import { useEffect, useRef } from 'react'
import { useVisibleWake } from './useVisibleWake'

// MAX_BACKOFF_MS caps the wait between polls of a failing endpoint.
export const MAX_BACKOFF_MS = 5 * 60 * 1000

// pollDelay is the wait before the next poll after `failures` failures in
// a row: the interval after none or one, then 2x, 4x and so on, capped at
// MAX_BACKOFF_MS but never shorter than the interval itself.
export function pollDelay(interval, failures) {
  if (failures <= 1) return interval
  return Math.max(interval, Math.min(interval * 2 ** (failures - 1), MAX_BACKOFF_MS))
}

// idlePoll is the poll state useAPI shares with usePolling: fetchData
// counts failures and calls schedule() after each answer.
export function idlePoll() {
  return { timer: null, dueAt: 0, failures: 0, paused: false, schedule: null, key: null }
}

// armPoll sets p.schedule for the current resource and returns its tick.
function armPoll(p, fetchData, intervalRef, active) {
  function tick() {
    p.timer = null
    if (document.hidden) {
      p.paused = true
      return
    }
    // Scheduled before the fetch too, so a request that never answers
    // is superseded by the next poll instead of stopping the polling.
    p.schedule()
    fetchData()
  }
  p.schedule = active ? () => {
    clearTimeout(p.timer)
    const delay = pollDelay(intervalRef.current, p.failures)
    p.dueAt = Date.now() + delay
    p.timer = setTimeout(tick, delay)
  } : null
  return tick
}

// startPoll fetches the resource `key` now (or, while backing off within
// the same resource, when its next poll falls due) and polls it from then
// on. It returns the effect cleanup, which also aborts a request in flight.
function startPoll(p, { key, fetchData, intervalRef, active, abortRef }) {
  // A moved time window is the same resource: it keeps its backoff.
  const sameResource = key !== null && p.key === key
  p.key = key
  if (!sameResource) p.failures = 0
  p.paused = false
  const tick = armPoll(p, fetchData, intervalRef, active)
  const wait = p.dueAt - Date.now()
  if (active && sameResource && p.failures > 1 && wait > 0) {
    // Backing off: the window moved, but the next poll keeps its time and
    // then asks for the window current by then.
    p.timer = setTimeout(tick, wait)
  } else {
    fetchData()
    p.schedule?.()
  }
  return () => {
    clearTimeout(p.timer)
    p.timer = null
    p.schedule = null
    if (abortRef.current) abortRef.current.abort()
  }
}

// wakePoll runs a poll skipped while hidden once the tab is shown, unless
// the same render already started a fetch (a time-range tick moved the url).
function wakePoll(p, fetchData, abortRef) {
  if (!p.paused || document.hidden) return
  p.paused = false
  if (!abortRef.current) fetchData()
}

// usePolling fetches `key`'s resource now and then every interval
// (dashboard-08): a poll that falls due while document.hidden is skipped
// and runs once when the tab is shown; failures in a row back off, also
// across time-window moves of the same resource. A changed interval
// applies from the next poll.
export function usePolling(poll, fetchData, { key, interval, active, abortRef }) {
  const intervalRef = useRef(interval)
  useEffect(() => { intervalRef.current = interval }, [interval])
  const wake = useVisibleWake()
  useEffect(() => startPoll(poll.current, { key, fetchData, intervalRef, active, abortRef }),
    [poll, fetchData, active, key, abortRef])
  useEffect(() => wakePoll(poll.current, fetchData, abortRef),
    [poll, wake, fetchData, abortRef])
}
