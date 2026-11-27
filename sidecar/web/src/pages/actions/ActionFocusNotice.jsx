// ActionFocusNotice: the Actions banner for a change record an Ask Sage
// citation focused (see actionFocus.js).

// ActionFocusNotice names the focused record, or says it is not loaded.
export function ActionFocusNotice({ id, found, allHref, onClear }) {
  return (
    <div data-testid="actions-action-focus" className="rounded border p-3 text-sm space-y-1"
      style={{ borderColor: 'var(--border)', background: 'var(--bg-card)' }}>
      <p>{found ? `Showing change record ${id}, cited by Ask Sage.`
        : `Change record ${id} is not among the loaded change records; widen the time ` +
          'range or check the database selection.'}</p>
      <a href={allHref} className="underline" style={{ color: 'var(--accent)' }}
        onClick={onClear}>Show all changes</a>
    </div>
  )
}
