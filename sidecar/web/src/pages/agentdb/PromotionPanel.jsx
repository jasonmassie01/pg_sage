import { useState } from 'react'
import { GitPullRequest } from 'lucide-react'

const initialForm = {
  title: '',
  migration_sql: '',
  verification_sql: '',
  rollback_sql: '',
}

function gateSummary(gates) {
  if (!gates || typeof gates !== 'object') return []
  return Object.entries(gates).slice(0, 4).map(([key, value]) =>
    `${key}: ${String(value)}`)
}

export function PromotionPanel({
  deployRequests,
  busy = false,
  onCreate,
  onApprove,
  onDeny,
}) {
  const [form, setForm] = useState(initialForm)

  function update(key, value) {
    setForm(current => ({ ...current, [key]: value }))
  }

  function submit(event) {
    event.preventDefault()
    onCreate?.({
      ...form,
      status: 'draft',
      risk_tier: 'moderate',
      created_by: 'operator',
    })
    setForm(initialForm)
  }

  return (
    <div data-testid="agent-db-promotion">
      <div className="mb-2 flex items-center gap-2">
        <GitPullRequest size={15} style={{ color: 'var(--text-secondary)' }} />
        <h3 className="text-xs font-semibold"
          style={{ color: 'var(--text-secondary)' }}>
          Promotion deploy requests
        </h3>
      </div>
      <form onSubmit={submit} className="mb-3 grid gap-2">
        <TextInput label="Promotion title" value={form.title}
          onChange={value => update('title', value)} />
        <TextArea label="Migration SQL" value={form.migration_sql}
          onChange={value => update('migration_sql', value)} />
        <TextArea label="Verification SQL" value={form.verification_sql}
          onChange={value => update('verification_sql', value)} />
        <TextArea label="Rollback SQL" value={form.rollback_sql}
          onChange={value => update('rollback_sql', value)} />
        <button type="submit" disabled={busy}
          data-testid="agent-db-create-deploy-request"
          className="w-fit rounded px-3 py-1.5 text-xs"
          style={{ background: 'var(--accent)', color: '#fff' }}>
          Draft promotion
        </button>
      </form>
      {deployRequests.length === 0 ? (
        <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
          None
        </p>
      ) : (
        <div className="space-y-2">
          {deployRequests.map(req => (
            <DeployRequestRow key={req.deploy_request_id} request={req}
              busy={busy} onApprove={onApprove} onDeny={onDeny} />
          ))}
        </div>
      )}
    </div>
  )
}

function DeployRequestRow({ request, busy, onApprove, onDeny }) {
  const gates = gateSummary(request.gate_results)
  return (
    <div className="rounded border p-2" style={{ borderColor: 'var(--border)' }}>
      <div className="flex items-start justify-between gap-2">
        <div>
          <div className="text-sm" style={{ color: 'var(--text-primary)' }}>
            {request.title}
          </div>
          <div className="text-xs" style={{ color: 'var(--text-secondary)' }}>
            {request.status} / {request.risk_tier}
          </div>
        </div>
        {request.status === 'review_requested' && (
          <div className="flex gap-1">
            <button type="button" disabled={busy}
              className="rounded border px-2 py-1 text-xs"
              style={{
                borderColor: 'var(--border)',
                color: 'var(--green)',
              }}
              onClick={() => onApprove?.(request.deploy_request_id)}>
              Approve
            </button>
            <button type="button" disabled={busy}
              className="rounded border px-2 py-1 text-xs"
              style={{
                borderColor: 'var(--border)',
                color: 'var(--red)',
              }}
              onClick={() => onDeny?.(request.deploy_request_id)}>
              Deny
            </button>
          </div>
        )}
      </div>
      {request.migration_sql && (
        <pre className="mt-2 overflow-auto rounded p-2 text-xs"
          style={{
            background: 'var(--bg-primary)',
            color: 'var(--text-secondary)',
          }}>
          {request.migration_sql}
        </pre>
      )}
      {gates.length > 0 && (
        <div className="mt-2 flex flex-wrap gap-1.5">
          {gates.map(item => (
            <span key={item} className="rounded border px-2 py-1 text-xs"
              style={{
                borderColor: 'var(--border)',
                color: 'var(--text-secondary)',
              }}>
              {item}
            </span>
          ))}
        </div>
      )}
    </div>
  )
}

function TextInput({ label, value, onChange }) {
  return (
    <label className="block text-xs" style={{ color: 'var(--text-secondary)' }}>
      <span>{label}</span>
      <input value={value} onChange={e => onChange(e.target.value)}
        className="mt-1 w-full rounded border px-2 py-1.5 text-sm"
        style={{
          background: 'var(--bg-primary)',
          borderColor: 'var(--border)',
          color: 'var(--text-primary)',
        }} />
    </label>
  )
}

function TextArea({ label, value, onChange }) {
  return (
    <label className="block text-xs" style={{ color: 'var(--text-secondary)' }}>
      <span>{label}</span>
      <textarea value={value} onChange={e => onChange(e.target.value)}
        rows={2}
        className="mt-1 w-full rounded border px-2 py-1.5 text-sm"
        style={{
          background: 'var(--bg-primary)',
          borderColor: 'var(--border)',
          color: 'var(--text-primary)',
        }} />
    </label>
  )
}
