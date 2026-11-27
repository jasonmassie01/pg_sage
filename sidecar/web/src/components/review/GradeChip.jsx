import { useAPI } from '../../hooks/useAPI'
import { gradeBadge, gradeURL, reviewHref } from '../../lib/review'

// GradeChip is a database's latest grade as a link to its Review page. It
// shows nothing before the first grade, while loading or on an error: the
// Review page itself says why. firstLook names the first grade the
// day-one grade.
export function GradeChip({ database, firstLook = false }) {
  const db = database && database !== 'all' ? database : null
  const { data } = useAPI(db ? gradeURL(db) : null, 60000)
  const latest = data?.latest
  if (!db || !latest) return null
  const badge = gradeBadge(latest.grade?.overall, latest.grade?.overall_score)
  const dayOne = firstLook && !data.previous && latest.triggers?.includes('first_look')
  const label = dayOne ? 'Day-one grade' : 'Grade'
  const letter = badge.unknown ? 'Unknown' : badge.letter
  return (
    <a data-testid="grade-chip" href={reviewHref(db)}
      aria-label={`${label} ${letter}: open the review of ${db}`}
      className="inline-flex items-center rounded px-2 py-0.5 text-xs font-medium underline"
      style={{ border: '1px solid var(--border)', color: 'var(--text-primary)' }}>
      {`${label}: ${letter}`}
    </a>
  )
}
