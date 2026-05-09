import {
  Activity, PlayCircle, ShieldCheck, TerminalSquare, Trash2,
} from 'lucide-react'

function formatDate(value) {
  if (!value) return 'n/a'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return 'n/a'
  return date.toLocaleString()
}

export function CloudProvisioningPanel({
  attempts,
  busy,
  onPreflight,
  onExecute,
  onStatus,
  onDestroyDryRun,
}) {
  return (
    <div className="rounded border p-3" data-testid="agent-db-provisioning"
      style={{ borderColor: 'var(--border)' }}>
      <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <TerminalSquare size={15} style={{ color: 'var(--text-secondary)' }} />
          <h3 className="text-xs font-semibold"
            style={{ color: 'var(--text-secondary)' }}>
            Cloud provisioning
          </h3>
        </div>
        <div className="flex flex-wrap gap-2">
          <IconButton label="Run preflight" icon={ShieldCheck}
            disabled={busy} onClick={onPreflight} />
          <IconButton label="Dry-run execute" icon={PlayCircle}
            disabled={busy} onClick={onExecute} />
          <IconButton label="Check status" icon={Activity}
            disabled={busy} onClick={onStatus} />
          <IconButton label="Destroy dry-run" icon={Trash2}
            disabled={busy} onClick={onDestroyDryRun} />
        </div>
      </div>
      <AttemptList attempts={attempts} />
    </div>
  )
}

function IconButton({ label, icon, onClick, disabled = false }) {
  const ButtonIcon = icon
  return (
    <button type="button" onClick={onClick} title={label} disabled={disabled}
      className="inline-flex items-center gap-1.5 rounded border px-2 py-1 text-xs"
      style={{ borderColor: 'var(--border)', color: 'var(--text-secondary)' }}>
      <ButtonIcon size={13} />
      {label}
    </button>
  )
}

function AttemptList({ attempts }) {
  if (!attempts || attempts.length === 0) {
    return (
      <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
        No provisioning attempts yet
      </p>
    )
  }
  return (
    <div className="space-y-2">
      {attempts.map(attempt => (
        <AttemptRow key={attempt.attempt_id} attempt={attempt} />
      ))}
    </div>
  )
}

function AttemptRow({ attempt }) {
  return (
    <div className="rounded border p-2" style={{ borderColor: 'var(--border)' }}>
      <div className="flex items-center justify-between gap-2">
        <span className="text-sm" style={{ color: 'var(--text-primary)' }}>
          {attempt.kind || 'provision'}
        </span>
        <span className="text-xs" style={{ color: 'var(--text-secondary)' }}>
          {attempt.status || 'recorded'}
        </span>
      </div>
      <div className="text-xs" style={{ color: 'var(--text-secondary)' }}>
        {formatDate(attempt.created_at)}
      </div>
      {attempt.stdout && (
        <pre className="mt-2 max-h-32 overflow-auto whitespace-pre-wrap rounded p-2 text-xs"
          style={{
            background: 'var(--bg-primary)',
            color: 'var(--text-primary)',
          }}>
          {attempt.stdout}
        </pre>
      )}
      {attempt.stderr && (
        <div className="mt-2 text-xs" style={{ color: 'var(--red)' }}>
          {attempt.stderr}
        </div>
      )}
    </div>
  )
}
