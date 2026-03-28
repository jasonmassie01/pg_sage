export function DeleteConfirm({ db, onConfirm, onCancel }) {
  return (
    <div className="fixed inset-0 flex items-center justify-center z-50"
      style={{ background: 'rgba(0,0,0,0.5)' }}>
      <div className="rounded-lg p-6 max-w-md w-full"
        style={{
          background: 'var(--bg-card)',
          border: '1px solid var(--border)',
        }}>
        <h3 className="text-sm font-semibold mb-2"
          style={{ color: 'var(--text-primary)' }}>
          Remove Database
        </h3>
        <p className="text-sm mb-4"
          style={{ color: 'var(--text-secondary)' }}>
          Remove database <strong
            style={{ color: 'var(--text-primary)' }}>
            {db.name}
          </strong>? This will stop monitoring.
        </p>
        <div className="flex gap-2 justify-end">
          <button onClick={onCancel}
            className="px-4 py-1.5 rounded text-sm"
            style={{ color: 'var(--text-secondary)' }}>
            Cancel
          </button>
          <button onClick={onConfirm}
            className="px-4 py-1.5 rounded text-sm font-medium"
            style={{ background: '#ef4444', color: '#fff' }}>
            Delete
          </button>
        </div>
      </div>
    </div>
  )
}
