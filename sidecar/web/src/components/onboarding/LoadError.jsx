// LoadError states a failed read with a Retry that fetches again.
export function LoadError({ what, error, onRetry }) {
  return (
    <div role="alert" className="flex items-center gap-3 text-sm"
      style={{ color: 'var(--text-secondary)' }}>
      <span>{what} unavailable: {error}</span>
      <button type="button" onClick={() => onRetry?.()}
        className="rounded border px-2 py-1 text-xs" style={{ borderColor: 'var(--border)' }}>
        Retry
      </button>
    </div>
  )
}
