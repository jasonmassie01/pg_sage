import { CheckCircle, Server } from 'lucide-react'
import { formatClock } from '../lib/time'

// cases-11: the hero may say no work is open only when every count the
// fleet summary carries is a known zero. A count the summary lacks is
// unknown, never zero.
const CASE_COUNT_KEYS = [
  'degraded', 'total_critical', 'total_warning', 'incidents_critical',
  'incidents_warning', 'incidents_unknown', 'stale_collection',
  'freshness_unknown',
]

function plural(n, word) {
  return `${n} ${word}${n === 1 ? '' : 's'}`
}

function dbCount(n) {
  return plural(n, 'database')
}

function count(summary, key) {
  return Number.isInteger(summary[key]) ? summary[key] : 0
}

// openWork lists the open work, critical first.
function openWork(summary) {
  return [
    ['incidents_critical', 'critical incident'],
    ['total_critical', 'critical finding'],
    ['incidents_warning', 'warning incident'],
    ['total_warning', 'warning finding'],
    ['degraded', 'degraded database'],
  ].map(([key, word]) => ({ n: count(summary, key), word }))
    .filter(w => w.n > 0)
    .map(w => ({ n: w.n, text: plural(w.n, w.word) }))
}

// evidenceGaps lists what keeps the counts from establishing health.
function evidenceGaps(summary) {
  const gaps = []
  if (CASE_COUNT_KEYS.some(key => !Number.isInteger(summary[key]))) {
    gaps.push('Some case counts are unavailable from this server.')
  }
  if (count(summary, 'incidents_unknown') > 0) {
    gaps.push('Incident counts could not be read on '
      + `${dbCount(summary.incidents_unknown)}.`)
  }
  if (count(summary, 'stale_collection') > 0) {
    gaps.push(`Collection is stale on ${dbCount(summary.stale_collection)}.`)
  }
  if (count(summary, 'freshness_unknown') > 0) {
    gaps.push('Collection freshness is unknown on '
      + `${dbCount(summary.freshness_unknown)}.`)
  }
  return gaps
}

// oldestCollection is the earliest last collection across databases: the
// whole Overview is no fresher than that.
function oldestCollection(databases) {
  const times = (databases || [])
    .map(db => Date.parse(db?.status?.collector_last_run))
    .filter(t => Number.isFinite(t) && t > 0)
  if (times.length === 0) return null
  return formatClock(Math.min(...times))
}

function ClearHero({ summary, databases }) {
  const last = oldestCollection(databases)
  return (
    <div data-testid="health-hero"
      className="rounded p-4 flex items-center gap-3"
      style={{
        background: 'rgba(34, 197, 94, 0.08)',
        border: '1px solid rgba(34, 197, 94, 0.3)',
      }}>
      <CheckCircle size={28} style={{ color: 'var(--green)' }} />
      <div>
        <div className="font-semibold" style={{ color: 'var(--green)' }}>
          No open critical or warning cases
        </div>
        <div className="text-sm" style={{ color: 'var(--text-secondary)' }}>
          Monitoring {dbCount(summary.total_databases)}
          {last ? `; last collection ${last}` : ''}.
        </div>
      </div>
    </div>
  )
}

function heroTitle(work) {
  if (work.length === 0) return 'Health not established'
  const issues = work.reduce((sum, w) => sum + w.n, 0)
  return issues === 1
    ? '1 Issue Needs Attention'
    : `${issues} Issues Need Attention`
}

export function HealthHero({ summary, databases }) {
  const work = openWork(summary)
  const gaps = evidenceGaps(summary)
  if (work.length === 0 && gaps.length === 0) {
    return <ClearHero summary={summary} databases={databases} />
  }
  const critical = count(summary, 'incidents_critical') > 0
    || count(summary, 'total_critical') > 0
  const tone = critical ? 'var(--red)' : 'var(--yellow)'
  return (
    <div data-testid="health-hero"
      className="rounded p-4 flex items-center gap-3"
      style={{
        background: critical
          ? 'rgba(239, 68, 68, 0.08)' : 'rgba(245, 158, 11, 0.08)',
        border: critical
          ? '1px solid rgba(239, 68, 68, 0.3)'
          : '1px solid rgba(245, 158, 11, 0.3)',
      }}>
      <Server size={28} style={{ color: tone }} />
      <div className="flex-1">
        <div className="font-semibold" style={{ color: tone }}>
          {heroTitle(work)}
        </div>
        {work.length > 0 && (
          <div className="text-sm" style={{ color: 'var(--text-secondary)' }}>
            {work.map(w => w.text).join(', ')}
          </div>
        )}
        {gaps.length > 0 && (
          <div className="text-sm" data-testid="health-hero-evidence-gap"
            style={{ color: 'var(--text-secondary)' }}>
            {gaps.join(' ')} These counts do not establish health.
          </div>
        )}
      </div>
      <a href="#/cases" data-testid="health-hero-cases-link"
        className="text-sm whitespace-nowrap" style={{ color: 'var(--accent)' }}>
        Review cases
      </a>
    </div>
  )
}
