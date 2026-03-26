export function LoadingSpinner() {
  return (
    <div className="flex items-center justify-center p-12">
      <div className="w-8 h-8 border-2 rounded-full animate-spin"
        style={{
          borderColor: 'var(--border)',
          borderTopColor: 'var(--accent)',
        }} />
    </div>
  )
}
