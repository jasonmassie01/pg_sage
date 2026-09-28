import { useAPI } from '../hooks/useAPI'

// D6: autonomous index builds earn load admission from IO evidence.
// This panel shows, per database, whether builds are admitted now, which
// evidence decided it, and how much of the IO baseline has been learned.

const MODE_LABELS = {
  declared_capacity: 'Declared capacity',
  learned_baseline: 'Learned baseline',
  unavailable: 'No IO evidence',
}

function admissionURL(database) {
  if (database && database !== 'all') {
    return `/api/v1/admission?database=${encodeURIComponent(database)}`
  }
  return '/api/v1/admission'
}

function BaselineProgress({ status }) {
  const required = status.baseline_required_days || 0
  if (status.mode !== 'learned_baseline' || required <= 0) return null
  const observed = Math.min(status.baseline_observed_days || 0, required)
  const pct = Math.round((observed / required) * 100)
  return (
    <div className="mt-2">
      <div
        role="progressbar"
        aria-label={`IO baseline for ${status.database}`}
        aria-valuemin={0}
        aria-valuemax={required}
        aria-valuenow={observed}
        className="h-1.5 w-full rounded"
        style={{ background: 'var(--border)' }}
      >
        <div
          className="h-1.5 rounded"
          style={{ width: `${pct}%`, background: 'var(--accent)' }}
        />
      </div>
      <div className="text-xs mt-1" style={{ color: 'var(--text-secondary)' }}>
        {observed.toFixed(1)} / {required} days of IO baseline
      </div>
    </div>
  )
}

function AdmissionRow({ status }) {
  const withheld = status.withheld_findings || 0
  const missing = status.missing_evidence || []
  return (
    <li
      data-testid={`index-admission-${status.database}`}
      className="py-2"
      style={{ borderTop: '1px solid var(--border)' }}
    >
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <span className="font-medium">{status.database}</span>
        <span style={{ color: status.ok ? 'var(--green)' : 'var(--yellow)' }}>
          {status.ok ? 'Admitted' : 'Withheld'}
        </span>
        <span style={{ color: 'var(--text-secondary)' }}>
          {MODE_LABELS[status.mode] || status.mode}
        </span>
      </div>
      {status.detail && (
        <div className="text-xs mt-1" style={{ color: 'var(--text-secondary)' }}>
          {status.detail}
        </div>
      )}
      {status.mode === 'unavailable' && missing.length > 0 && (
        <div className="text-xs mt-1" style={{ color: 'var(--text-secondary)' }}>
          Missing evidence: {missing.join(', ')}
        </div>
      )}
      <BaselineProgress status={status} />
      {withheld > 0 && (
        <div className="text-xs mt-1" style={{ color: 'var(--text-secondary)' }}>
          {withheld} {withheld === 1 ? 'finding' : 'findings'} withheld
        </div>
      )}
    </li>
  )
}

export function IndexAdmissionPanel({ database }) {
  const { data } = useAPI(admissionURL(database), 60000)
  const databases = data?.databases || []
  if (databases.length === 0) return null
  return (
    <section
      data-testid="index-admission-panel"
      className="rounded p-3"
      style={{ background: 'var(--bg-card)', border: '1px solid var(--border)' }}
    >
      <h3 className="text-sm font-semibold">Autonomous index builds</h3>
      <p className="text-xs mt-1" style={{ color: 'var(--text-secondary)' }}>
        Admitted only on IO evidence: a declared capacity, or a learned
        baseline and a quiet period.
      </p>
      <ul className="mt-2">
        {databases.map((status) => (
          <AdmissionRow key={status.database} status={status} />
        ))}
      </ul>
    </section>
  )
}
