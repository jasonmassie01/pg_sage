// Scorecard payload (review-05b): review-05a's Go handler golden, GET
// /api/v1/databases/{db}/review/scorecard as
// TestReviewScorecardRouteDB_NumbersGateAndGolden builds it. It is read from
// internal/api/testdata/review_scorecard.golden.json itself, so the mock
// cannot drift from the route. The golden masks ids and times; the mock
// fills them in.

import golden from '../../../internal/api/testdata/review_scorecard.golden.json'
  with { type: 'json' }

type GateRow = { rule: string, verdict: string, id: unknown, [key: string]: unknown }

// reviewScorecard is the golden with real ids and times, and the G-M1 row
// in the given verdict.
export function reviewScorecard(verdict = 'pending_approval') {
  const base = structuredClone(golden) as { gate: GateRow[], [key: string]: unknown }
  base.gate = base.gate.map((row, i) => ({
    ...row, id: 7 + i, tick: '2026-11-16T10:00:00Z', evaluated_at: '2026-11-16T10:00:05Z',
    ...(row.rule === 'G-M1' ? { verdict } : {}),
  }))
  return base
}
