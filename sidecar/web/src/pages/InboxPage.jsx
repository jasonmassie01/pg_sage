import { useEffect, useState } from 'react'
import { useAPI } from '../hooks/useAPI'
import { QueryState } from '../components/QueryState'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { WhyNotButton } from '../components/WhyNotPanel'
import { usePendingActionsRefetch } from '../components/Layout'
import { apiGet } from '../lib/api'
import {
  changesSince, goalSentence, ledgerGaps, sinceLabel, sinceWindow, sourceErrorText,
} from '../lib/inbox'
import { NeedsYou } from './inbox/NeedsYou'
import { SinceLastVisit } from './inbox/SinceLastVisit'

// The Inbox is the home page (ux-01b): (a) what needs a person, from
// /api/v1/inbox; (b) what changed since they were last here, from the
// outcome ledger since their inbox_seen_at; (c) one Value line. The empty
// state is the goal state, worded from counts. A failed source is named
// and the goal state is then never claimed.

const SEEN_URL = '/api/v1/inbox/seen'
// SEEN_AFTER_MS: the page marks itself seen after this long visible.
const SEEN_AFTER_MS = 5000
const muted = { color: 'var(--text-secondary)' }
const dbParam = (database, sep) => (database && database !== 'all'
  ? `${sep}database=${encodeURIComponent(database)}` : '')

// useSeenTime reads the person's seen time once, before anything marks it.
// No control pool (404) is a first visit; another failure says so.
function useSeenTime() {
  const [seen, setSeen] = useState({ ready: false, seenAt: null, failed: false, canMark: true })
  useEffect(() => {
    let live = true
    apiGet(SEEN_URL)
      .then(data => live && setSeen({ ready: true, seenAt: data?.seen_at ?? null,
        failed: false, canMark: true }))
      .catch(err => live && setSeen({ ready: true, seenAt: null,
        failed: err?.status !== 404, canMark: err?.status !== 404 }))
    return () => { live = false }
  }, [])
  return seen
}

async function postSeen(openedAt) {
  const res = await fetch(SEEN_URL, {
    method: 'POST', credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ seen_at: new Date(openedAt).toISOString() }),
  })
  if (!res.ok) throw new Error(`could not record this visit (${res.status})`)
}

// useMarkSeen marks the page seen, once, after SEEN_AFTER_MS visible. The
// time sent is when the page opened, so a change that landed while it was
// open is shown again next time rather than missed.
function useMarkSeen(openedAt, enabled) {
  const [error, setError] = useState(null)
  useEffect(() => {
    if (!enabled) return undefined
    let timer = null
    let done = false
    const arm = () => {
      clearTimeout(timer)
      if (done || document.visibilityState !== 'visible') return
      timer = setTimeout(() => {
        done = true
        postSeen(openedAt).catch(err => setError(err.message))
      }, SEEN_AFTER_MS)
    }
    arm()
    document.addEventListener('visibilitychange', arm)
    return () => {
      clearTimeout(timer)
      document.removeEventListener('visibilitychange', arm)
    }
  }, [openedAt, enabled])
  return error
}

function windowLabel(seen, win, openedAt) {
  if (seen.failed) return 'Could not read when you were last here: changes in the last 24 hours.'
  if (win.firstVisit) return 'First visit: changes in the last 24 hours.'
  const label = sinceLabel(win.from, openedAt)
  return `Changes ${label}.`
}

function hours(n) {
  return `${Number(n || 0).toLocaleString(undefined, { maximumFractionDigits: 1 })} h`
}

function ValueLine({ query }) {
  const saved = query.data?.dba_hours_saved
  if (!saved) return null
  return <p className="text-sm" data-testid="inbox-value" style={muted}>
    Verified DBA hours saved: {hours(saved.this_week)} this week, {hours(saved.all_time)} all
    time. <a href="#/value" className="underline" style={{ color: 'var(--accent)' }}>
      Open Value</a>
  </p>
}

// NeedsSection is (a), or the goal state when nothing waits and every
// source answered. The heading counts every item found (total), which a
// cut-short list (truncated) does not all show.
function NeedsSection({ data, changes, when, partial, onDecided, database, user }) {
  const items = Array.isArray(data?.items) ? data.items : []
  const total = Number.isInteger(data?.total) ? Math.max(data.total, items.length) : items.length
  const failed = sourceErrorText(data?.errors)
  return <section className="space-y-2" aria-label="Needs you">
    {failed && <p role="status" data-testid="inbox-source-errors" className="text-sm"
      style={{ color: 'var(--yellow)' }}>{failed}</p>}
    {items.length > 0 && <>
      <h2 className="text-sm font-semibold" style={{ color: 'var(--text-primary)' }}>
        Needs you ({total}{data?.lower_bound ? '+' : ''})</h2>
      {total > items.length && <p data-testid="inbox-truncated" className="text-xs" style={muted}>
        Showing the first {items.length} of {total}; open each page for the rest.</p>}
      <NeedsYou items={items} onDecided={onDecided} />
    </>}
    {items.length === 0 && (failed
      ? <p className="text-sm" style={muted}>Nothing waits in the sources that answered.</p>
      : <>
        <p data-testid="inbox-goal" className="text-base font-medium"
          style={{ color: 'var(--text-primary)' }}>{goalSentence(changes, when, partial)}</p>
        {database && database !== 'all' && <WhyNotButton user={user} database={database}
          label="Nothing proposed? See why" />}
      </>)}
  </section>
}

export function InboxPage({ database, user }) {
  const [openedAt] = useState(() => Date.now())
  const seen = useSeenTime()
  const markError = useMarkSeen(openedAt, seen.ready && seen.canMark)
  const refetchBadge = usePendingActionsRefetch()
  const inbox = useAPI(`/api/v1/inbox${dbParam(database, '?')}`, 30000)
  const win = sinceWindow(seen.seenAt, openedAt)
  const outcomes = useAPI(seen.ready
    ? `/api/v1/outcomes?since=${win.since}${dbParam(database, '&')}` : null, 30000)
  const value = useAPI(`/api/v1/value${dbParam(database, '?')}`, 60000)
  const changes = changesSince(outcomes.data?.rows, win.from)
  const gaps = ledgerGaps(outcomes.data, win.from)
  const when = win.firstVisit ? 'in the last 24 hours' : sinceLabel(win.from, openedAt)
  const canUndo = user?.role === 'admin' || user?.role === 'operator'
  const onDecided = () => {
    inbox.refetch?.()
    refetchBadge()
  }
  return (
    <div className="space-y-6" data-testid="inbox-page">
      <QueryState query={inbox} skeleton={<LoadingSpinner />}>
        {data => <NeedsSection data={data} changes={changes} when={when}
          partial={gaps.partial} onDecided={onDecided} database={database} user={user} />}
      </QueryState>
      {seen.ready && <SinceLastVisit changes={changes} query={outcomes} canUndo={canUndo}
        gaps={gaps} label={windowLabel(seen, win, openedAt)} />}
      {markError && <p className="text-xs" style={muted}>{markError}</p>}
      <ValueLine query={value} />
    </div>
  )
}
