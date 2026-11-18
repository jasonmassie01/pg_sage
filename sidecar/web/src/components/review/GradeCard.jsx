import { LetterBadge } from './LetterBadge'
import { overallUnknownReason } from '../../lib/review'
import { formatTime } from '../../lib/time'

const card = { background: 'var(--bg-card)', border: '1px solid var(--border)' }
const muted = { color: 'var(--text-secondary)' }

// GradeCard is a graded run's overall letter, coverage and rubric version.
export function GradeCard({ run }) {
  const grade = run?.grade || {}
  return (
    <section data-testid="grade-card" className="rounded p-4 space-y-2" style={card}
      aria-labelledby="grade-card-heading">
      <h2 id="grade-card-heading" className="text-sm font-medium" style={muted}>
        Overall grade
      </h2>
      <LetterBadge large letter={grade.overall} score={grade.overall_score}
        reason={overallUnknownReason(grade)} />
      <p className="text-sm">
        Coverage {Number(grade.coverage_pct || 0)}% · Rubric {grade.rubric_version
          || run?.rubric_version}
      </p>
      <p className="text-xs" style={muted}>
        Graded {formatTime(run?.finished_at)}
        {run?.status === 'partial'
          ? ' · Partial: a producer could not be read, so some checks are unknown.' : ''}
      </p>
    </section>
  )
}
