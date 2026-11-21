import { Inbox } from 'lucide-react'

export function EmptyState({ message, action, actionLabel, icon: Icon }) {
  const DisplayIcon = Icon || Inbox
  return (
    <div className="flex flex-col items-center justify-center p-12 text-center gap-3">
      <DisplayIcon size={32} style={{ color: 'var(--text-secondary)', opacity: 0.5 }} />
      <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
        {message || 'No data available'}
      </p>
      {action && actionLabel && (
        <a href={action}
          className="px-4 py-2 rounded text-sm font-medium mt-2"
          style={{ background: 'var(--accent-solid)', color: '#fff' }}>
          {actionLabel}
        </a>
      )}
    </div>
  )
}
