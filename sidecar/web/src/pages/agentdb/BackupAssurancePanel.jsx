import { RotateCcw, ShieldCheck } from 'lucide-react'

function formatDate(value) {
  if (!value) return 'n/a'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return 'n/a'
  return date.toLocaleString()
}

function backupStatusColor(status) {
  switch (status) {
    case 'restore_verified':
    case 'verified':
      return 'var(--green)'
    case 'failed':
    case 'missing':
      return 'var(--red)'
    default:
      return 'var(--text-secondary)'
  }
}

export function BackupAssurancePanel({
  backups,
  busy = false,
  onCheckBackups,
  onPlanRestoreDrill,
}) {
  return (
    <div className="rounded border p-3 space-y-3"
      data-testid="agent-db-backup-assurance"
      style={{ borderColor: 'var(--border)' }}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <ShieldCheck size={15} style={{ color: 'var(--text-secondary)' }} />
          <h3 className="text-xs font-semibold"
            style={{ color: 'var(--text-secondary)' }}>
            Backup assurance
          </h3>
        </div>
        <div className="flex flex-wrap gap-2">
          <AssuranceButton
            label="Check backups"
            icon={ShieldCheck}
            disabled={busy}
            onClick={onCheckBackups}
          />
          <AssuranceButton
            label="Plan restore drill"
            icon={RotateCcw}
            disabled={busy}
            onClick={onPlanRestoreDrill}
          />
        </div>
      </div>
      {backups.length === 0 ? (
        <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
          No backup records
        </p>
      ) : (
        <div className="space-y-2">
          {backups.map(backup => (
            <div key={backup.backup_id} className="rounded border p-2"
              style={{ borderColor: 'var(--border)' }}>
              <div className="flex items-center justify-between gap-2">
                <span className="truncate text-sm"
                  style={{ color: 'var(--text-primary)' }}>
                  {backup.backup_id}
                </span>
                <span className="text-xs font-medium"
                  style={{ color: backupStatusColor(backup.status) }}>
                  {backup.status || 'unknown'}
                </span>
              </div>
              <div className="text-xs" style={{ color: 'var(--text-secondary)' }}>
                {backup.provider || 'unknown'} /{' '}
                {formatDate(backup.verified_at || backup.created_at)}
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

function AssuranceButton({ label, icon, disabled, onClick }) {
  const ButtonIcon = icon
  return (
    <button type="button" onClick={onClick} disabled={disabled}
      className="inline-flex items-center gap-1.5 rounded border px-2 py-1 text-xs"
      style={{ borderColor: 'var(--border)', color: 'var(--text-secondary)' }}>
      <ButtonIcon size={13} />
      {label}
    </button>
  )
}
