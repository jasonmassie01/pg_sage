import { AlertTriangle, RefreshCw } from 'lucide-react'

export function ErrorBanner({ message, onRetry }) {
  return (
    <div className="flex items-center gap-3 p-4 rounded"
      style={{
        background: '#3b1111',
        border: '1px solid var(--red)',
      }}>
      <AlertTriangle size={18} style={{ color: 'var(--red)' }} />
      <span className="flex-1 text-sm" style={{ color: 'var(--red)' }}>
        {message}
      </span>
      {onRetry && (
        <button onClick={onRetry} className="p-1.5 rounded"
          style={{ color: 'var(--text-secondary)' }}>
          <RefreshCw size={14} />
        </button>
      )}
    </div>
  )
}
