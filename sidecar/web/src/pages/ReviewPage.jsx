import { useEffect, useRef, useState } from 'react'
import { useAPI } from '../hooks/useAPI'
import { apiSend } from '../lib/api'
import { gradeURL, reviewParams, viewedURL } from '../lib/review'
import { formatTime } from '../lib/time'
import { EmptyState } from '../components/EmptyState'
import { QueryState } from '../components/QueryState'
import { GradeCard } from '../components/review/GradeCard'
import { DimensionTable } from '../components/review/DimensionTable'
import { ReviewDelta } from '../components/review/ReviewDelta'
import { ReviewReport } from '../components/review/ReviewReport'
import { ReviewScorecard } from '../components/review/ReviewScorecard'
import { useReviewScorecard } from '../hooks/useReviewScorecard'

// ReviewPage (review-01b) is one database's Sage Review: the latest grade,
// its dimensions, what changed since the previous grade and the report.
// The database comes from ?db= (the grade chips link there), else the
// picker. While a run is in progress the page polls every 5 s and shows a
// banner; the next poll after it finishes shows the new report. Under the
// report is the scorecard and the model gate (review-05b), read on its own.
//
// Opening a finished review sends its read receipt once (POST
// .../review/runs/{id}/viewed), unless the server says this person already
// opened it. A failed receipt is retried once and never blocks the page.

const RUNNING_POLL_MS = 5000
const IDLE_POLL_MS = 60000
const VIEW_RETRY_MS = 1000

export function ReviewPage({ route, database, user }) {
  const db = reviewParams(route, database)
  const [interval, setPollInterval] = useState(IDLE_POLL_MS)
  const scorecard = useReviewScorecard(db)
  const query = useAPI(db ? gradeURL(db) : null, interval)
  // Adjusted during render (not in an effect): the next poll uses it.
  const wanted = query.data?.running ? RUNNING_POLL_MS : IDLE_POLL_MS
  if (wanted !== interval) setPollInterval(wanted)
  useReadReceipt(db, query.data)
  if (!db) return <EmptyState message="Select a database to see its review." />
  return (
    <QueryState query={query}>
      {data => <ReviewBody data={data} database={db} user={user} scorecard={scorecard} />}
    </QueryState>
  )
}

function ReviewBody({ data, database, user, scorecard }) {
  const latest = data?.latest
  return (
    <div className="space-y-4">
      {data?.running && (
        <p role="status" data-testid="review-running" className="rounded px-3 py-2 text-sm"
          style={{ border: '1px solid var(--accent)' }}>
          A grade is running since {formatTime(data.running.started_at)}. This page updates
          when it finishes.
        </p>
      )}
      {!latest && !data?.running && (
        <EmptyState message="First grade runs after the first look." />
      )}
      {latest && (
        <>
          <GradeCard run={latest} />
          <DimensionTable dimensions={latest.grade?.dimensions} />
          <ReviewDelta delta={data.delta} previous={data.previous} />
          <ReviewReport report={data.report} database={database} />
          <ReviewScorecard query={scorecard} database={database} user={user} />
        </>
      )}
    </div>
  )
}

// useReadReceipt posts the receipt of the latest finished grade once per
// database and run for this page, unless viewed_at shows it was sent.
function useReadReceipt(db, data) {
  const sent = useRef(new Set())
  const runID = data?.latest?.id
  const viewed = Boolean(data?.viewed_at)
  useEffect(() => {
    if (!db || !runID || viewed) return undefined
    const key = `${db}#${runID}`
    if (sent.current.has(key)) return undefined
    sent.current.add(key)
    let timer = null
    // An empty JSON body: the server refuses a POST without a JSON
    // Content-Type (415), and apiSend sets it only when there is a body.
    const send = () => apiSend('POST', viewedURL(db, runID), {})
    send().catch(() => {
      timer = setTimeout(() => send().catch(err => {
        console.warn(`review read receipt of run ${runID} on ${db} failed twice:`, err)
      }), VIEW_RETRY_MS)
    })
    return () => clearTimeout(timer)
  }, [db, runID, viewed])
}
