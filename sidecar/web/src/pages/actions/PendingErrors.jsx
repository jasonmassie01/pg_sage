// PendingErrors lists databases whose queue could not be read, so a
// failure is never shown as "nothing waiting" (G9-B15).
export function PendingErrors({ errors }) {
  if (!Array.isArray(errors) || errors.length === 0) return null
  return (
    <div data-testid="pending-errors" role="alert"
      className="p-2 rounded text-sm"
      style={{
        border: '1px solid var(--yellow)',
        color: 'var(--yellow)',
      }}>
      Could not read pending actions from:{' '}
      {errors.map(e => `${e.database} (${e.error})`).join(', ')}.
      Approvals from these databases may be missing.
    </div>
  )
}
