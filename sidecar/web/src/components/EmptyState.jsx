export function EmptyState({ message }) {
  return (
    <div className="flex items-center justify-center p-12 text-center">
      <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
        {message || 'No data available'}
      </p>
    </div>
  )
}
