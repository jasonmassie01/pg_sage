import { useAPI } from './useAPI'
import { scorecardDatabase, scorecardURL } from '../lib/scorecard'

const POLL_MS = 60000

// useReviewScorecard reads one database's scorecard (review-05b), polled
// each minute; the whole fleet reads nothing. A page calls it before its
// own reads, so its own read stays the last one it makes.
export function useReviewScorecard(database) {
  const db = scorecardDatabase(database)
  return useAPI(db ? scorecardURL(db) : null, POLL_MS)
}
