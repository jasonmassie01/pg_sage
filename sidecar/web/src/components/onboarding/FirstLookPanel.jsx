import { useAPI } from '../../hooks/useAPI'
import { entryErrorText } from './entryError'
import { LoadError } from './LoadError'
import { RecheckButton } from './RecheckButton'
import { useRecheckedRefetch } from './recheck'
import { GradeChip } from '../review/GradeChip'

// FirstLookPanel shows the catalog-only first look: what a DBA would flag
// on day one, each finding with the catalog evidence it rests on. Suggested
// SQL is for the operator to review; pg_sage never runs it.

const severityColor = {
  critical: 'var(--red)',
  warning: 'var(--yellow)',
  info: 'var(--text-secondary)',
}

function firstLookURL(database) {
  if (!database || database === 'all') return '/api/v1/first-look'
  return `/api/v1/first-look?database=${encodeURIComponent(database)}`
}

function Evidence({ evidence }) {
  if (!evidence?.length) return null
  return (
    <details className="mt-2 text-xs">
      <summary className="cursor-pointer" style={{ color: 'var(--text-secondary)' }}>
        Evidence
      </summary>
      <ul className="mt-1 space-y-1 font-mono">
        {evidence.map((e, i) => (
          <li key={i}>{e.ref}{e.detail ? ` — ${e.detail}` : ''}</li>
        ))}
      </ul>
    </details>
  )
}

function FindingCard({ item }) {
  return (
    <article className="rounded border p-3"
      style={{ borderColor: 'var(--border)', background: 'var(--bg-card)' }}>
      <div className="flex items-start gap-2">
        <span className="text-xs font-semibold uppercase"
          style={{ color: severityColor[item.severity] || 'var(--text-secondary)' }}>
          {item.severity}
        </span>
        <h3 className="text-sm font-semibold">{item.title}</h3>
      </div>
      {item.detail && <p className="mt-1 text-sm">{item.detail}</p>}
      {item.recommendation && (
        <p className="mt-1 text-sm" style={{ color: 'var(--text-secondary)' }}>
          {item.recommendation}
        </p>
      )}
      {item.caveat && (
        <p className="mt-1 text-xs italic" style={{ color: 'var(--text-secondary)' }}>
          {item.caveat}
        </p>
      )}
      {item.suggested_sql && (
        <div className="mt-2">
          <pre className="overflow-x-auto rounded p-2 text-xs"
            style={{ background: 'var(--bg-primary)' }}>{item.suggested_sql}</pre>
          <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
            Review before running it yourself: pg_sage never runs it.
          </p>
        </div>
      )}
      <Evidence evidence={item.evidence} />
    </article>
  )
}

const POSTURE = 'agent_posture'

// PostureSection is the "Agent posture" section: how exposed the database
// is to agents and untrusted clients. Every fix is a script to review.
function PostureSection({ report }) {
  const checks = (report.checks || []).filter(c => c.section === POSTURE)
  if (checks.length === 0) return null
  const items = (report.items || []).filter(i => i.section === POSTURE)
  const withFindings = checks.filter(c => c.status === 'finding').length
  const degraded = checks.filter(c => c.status === 'degraded').length
  let summary = `${checks.length} checks, ${withFindings} with findings`
  if (degraded > 0) summary += `, ${degraded} could not run`
  return (
    <section className="space-y-2" aria-labelledby={`posture-${report.database}`}>
      <h3 id={`posture-${report.database}`} className="text-sm font-semibold">
        Agent posture
      </h3>
      <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>{summary}.</p>
      {items.length === 0 ? (
        <p className="text-sm">No agent posture problems found.</p>
      ) : items.map((item, i) => (
        <FindingCard key={`${item.rule}-${item.object}-${i}`} item={item} />
      ))}
    </section>
  )
}

// countOf is n with word, plural unless n is 1.
function countOf(n, word) {
  return `${n} ${word}${n === 1 ? '' : 's'}`
}

// splitNote separates a check's note from the GRANT that fixes it, so the
// SQL can be shown (and copied) on its own.
function splitNote(note) {
  const text = note || ''
  const at = text.search(/\bGRANT /)
  if (at < 0) return { prose: text, sql: '' }
  return { prose: text.slice(0, at).replace(/[\s;:,]+$/, ''), sql: text.slice(at).trim() }
}

function CheckRow({ check }) {
  const { prose, sql } = splitNote(check.note)
  const degraded = check.status === 'degraded'
  return (
    <li className="rounded border p-2 text-sm"
      style={{ borderColor: 'var(--border)', background: 'var(--bg-card)' }}>
      <div className="flex items-start gap-2">
        <span className="text-xs font-semibold uppercase"
          style={{ color: degraded ? 'var(--yellow)' : 'var(--text-secondary)' }}>
          {degraded ? 'could not run' : 'partial'}
        </span>
        <span className="font-mono text-xs">{check.rule}</span>
      </div>
      {prose && <p className="mt-1">{prose}</p>}
      {sql && (
        <pre className="mt-1 overflow-x-auto rounded p-2 text-xs"
          style={{ background: 'var(--bg-primary)' }}>{sql}</pre>
      )}
    </li>
  )
}

// ChecksStrip lists the catalog checks that could not run (first) and those
// that found more than the report shows, each with its reason and fix.
function ChecksStrip({ database, checks }) {
  const degraded = checks.filter(c => c.status === 'degraded')
  const truncated = checks.filter(c => c.status !== 'degraded' && c.truncated)
  if (degraded.length === 0 && truncated.length === 0) return null
  let summary = countOf(checks.length, 'check')
  if (degraded.length > 0) summary += `, ${degraded.length} could not run`
  if (truncated.length > 0) summary += `, ${truncated.length} not shown in full`
  return (
    <section className="space-y-2" aria-labelledby={`checks-${database}`}>
      <h3 id={`checks-${database}`} className="text-sm font-semibold">Catalog checks</h3>
      <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>{summary}.</p>
      <ul className="space-y-2">
        {[...degraded, ...truncated].map(c => <CheckRow key={c.rule} check={c} />)}
      </ul>
    </section>
  )
}

// emptyText is what an empty catalog item list means: healthy only when
// every catalog check ran.
function emptyText(checks) {
  const degraded = checks.filter(c => c.status === 'degraded').length
  if (degraded === 0) return 'No problems found in the catalog.'
  const ran = checks.length - degraded
  if (ran === 0) {
    if (degraded === 1) return 'The one catalog check could not run, so nothing is known yet.'
    return `None of the ${degraded} catalog checks could run, so nothing is known yet.`
  }
  const that = ran === 1 ? 'check that ran' : 'checks that ran'
  return `No problems found in the ${ran} ${that}; ${degraded} could not run.`
}

function Report({ entry, user }) {
  const report = entry.report
  if (entry.error) {
    return (
      <p className="text-sm" style={{ color: 'var(--yellow)' }}>
        {entryErrorText(entry.error, entry.database)}
      </p>
    )
  }
  if (!report) {
    return (
      <p role="status" className="text-sm" style={{ color: 'var(--text-secondary)' }}>
        First look running for {entry.database}…
      </p>
    )
  }
  const items = (report.items || []).filter(i => i.section !== POSTURE)
  const checks = (report.checks || []).filter(c => c.section !== POSTURE)
  return (
    <div className="space-y-2">
      <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
        {entry.database}: {Number(report.relations || 0).toLocaleString('en-US')} relations
        read in {report.duration_ms} ms, catalog only.
        <RecheckButton database={entry.database} user={user} />
      </p>
      <GradeChip database={entry.database} firstLook />
      {report.summary && <p className="text-sm font-medium">{report.summary}</p>}
      {items.length === 0 ? (
        <p className="text-sm">{emptyText(checks)}</p>
      ) : items.map((item, i) => (
        <FindingCard key={`${item.rule}-${item.object}-${i}`} item={item} />
      ))}
      <ChecksStrip database={report.database} checks={checks} />
      <PostureSection report={report} />
    </div>
  )
}

export function FirstLookPanel({ database, user }) {
  const { data, error, refetch } = useAPI(firstLookURL(database), 30000)
  useRecheckedRefetch(refetch)
  if (error && !data) {
    return (
      <section className="space-y-3" aria-labelledby="first-look-heading">
        <h2 id="first-look-heading" className="text-lg font-semibold">First look</h2>
        <LoadError what="First look" error={error} onRetry={refetch} />
      </section>
    )
  }
  const entries = data?.databases || []
  if (entries.length === 0) return null
  return (
    <section className="space-y-3" aria-labelledby="first-look-heading">
      <h2 id="first-look-heading" className="text-lg font-semibold">First look</h2>
      {entries.map(entry => <Report key={entry.database} entry={entry} user={user} />)}
    </section>
  )
}
