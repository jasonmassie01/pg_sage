
// DoctorReport shows GET /api/v1/doctor for the connected database (ux-04a)
// in the setup wizard's Connect step. Only a failed connection check
// blocks Next: warnings and other failures say what to fix and let the
// operator go on (ux-04b).

const STATUS_COLOR = { pass: 'var(--green)', warn: 'var(--yellow)', fail: 'var(--red, #ef4444)',
  unknown: 'var(--text-secondary)' }

function Check({ check }) {
  return (
    <li data-testid={`doctor-check-${check.id}`} data-status={check.status} className="text-sm">
      <span className="font-medium" style={{ color: STATUS_COLOR[check.status] }}>
        {check.status}
      </span>{' '}
      <span style={{ color: 'var(--text-primary)' }}>{check.title}</span>
      {check.detail && (
        <span style={{ color: 'var(--text-secondary)' }}>: {check.detail}</span>
      )}
      {check.fix && (
        <code className="block text-xs font-mono mt-0.5" style={{ color: 'var(--text-primary)' }}>
          {check.fix}
        </code>
      )}
    </li>
  )
}

// DoctorReport shows useDoctor's state for database.
export function DoctorReport({ database, doctor }) {
  const { loading, report, error } = doctor
  if (loading) return <p className="text-sm">Running the doctor on {database}...</p>
  if (error) {
    return (
      <p role="alert" className="text-sm" style={{ color: 'var(--text-secondary)' }}>
        The doctor report could not be read ({error}); pg_sage is connected, so you can go on.
      </p>
    )
  }
  return (
    <section aria-label="Doctor report" className="space-y-1">
      <h3 className="text-sm font-semibold" style={{ color: 'var(--text-primary)' }}>
        Doctor report for {database}
      </h3>
      <ul className="space-y-1">
        {(report?.checks || []).map(c => <Check key={c.id} check={c} />)}
      </ul>
    </section>
  )
}
