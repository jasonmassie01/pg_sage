import { QueryState } from '../QueryState'
import { scorecardDatabase } from '../../lib/scorecard'
import { Scorecard } from './Scorecard'

const card = { background: 'var(--bg-card)', border: '1px solid var(--border)' }
const muted = { color: 'var(--text-secondary)' }

// ReviewScorecard (review-05b) shows a scorecard read (useReviewScorecard)
// in its own QueryState, so a scorecard that cannot be read is named here
// and the page around it still renders. Without a database it asks for one.
export function ReviewScorecard({ query, database, user, compact = false }) {
  const db = scorecardDatabase(database)
  return (
    <section data-testid="review-scorecard" className="rounded p-4 space-y-3" style={card}
      aria-labelledby="scorecard-heading">
      <h2 id="scorecard-heading" className="text-sm font-medium" style={muted}>
        Review scorecard
      </h2>
      {db && query ? (
        <QueryState query={query}>
          {data => <Scorecard data={data} user={user} compact={compact} database={db} />}
        </QueryState>
      ) : (
        <p className="text-sm">Select a database to see its review scorecard.</p>
      )}
    </section>
  )
}
