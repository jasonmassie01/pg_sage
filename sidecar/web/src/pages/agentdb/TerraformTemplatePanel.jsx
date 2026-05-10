import { useState } from 'react'
import { FileCode, Upload } from 'lucide-react'
import { FieldTip } from './AgentDBFormControls'

const PROVIDERS = [
  { key: 'aws_rds', label: 'AWS RDS' },
  { key: 'gcp_cloudsql', label: 'Cloud SQL' },
  { key: 'databricks_lakebase', label: 'Lakebase' },
]

export function TerraformTemplatePanel({
  templates,
  busy,
  onUpload,
  onApprove,
  onProvision,
}) {
  const [form, setForm] = useState({
    template_id: '',
    provider: 'aws_rds',
    path: 'main.tf',
    content: '',
  })

  async function submit(event) {
    event.preventDefault()
    await onUpload({
      template_id: form.template_id,
      provider: form.provider,
      files: [{ path: form.path, body: form.content }],
    })
  }

  return (
    <section className="rounded border p-4 space-y-4"
      data-testid="agent-db-terraform-template"
      style={{ background: 'var(--bg-card)', borderColor: 'var(--border)' }}>
      <div className="flex items-center gap-2">
        <FileCode size={16} style={{ color: 'var(--accent)' }} />
        <h2 className="text-sm font-semibold"
          style={{ color: 'var(--text-primary)' }}>
          Terraform Templates
        </h2>
      </div>
      <form onSubmit={submit} className="grid gap-3 md:grid-cols-2">
        <TextField label="Template ID" value={form.template_id}
          tipKey="terraform_template_id"
          onChange={value => setForm({ ...form, template_id: value })} />
        <label className="block text-xs" style={{ color: 'var(--text-secondary)' }}>
          <FieldTip tipKey="terraform_provider">
            <span>Template provider</span>
          </FieldTip>
          <select value={form.provider}
            onChange={e => setForm({ ...form, provider: e.target.value })}
            className="mt-1 w-full rounded border px-2 py-1.5 text-sm"
            style={inputStyle()}>
            {PROVIDERS.map(option => (
              <option key={option.key} value={option.key}>
                {option.label}
              </option>
            ))}
          </select>
        </label>
        <TextField label="Terraform file name" value={form.path}
          tipKey="terraform_path"
          onChange={value => setForm({ ...form, path: value })} />
        <label className="block text-xs md:col-span-2"
          style={{ color: 'var(--text-secondary)' }}>
          <FieldTip tipKey="terraform_content">
            <span>Terraform content</span>
          </FieldTip>
          <textarea value={form.content}
            onChange={e => setForm({ ...form, content: e.target.value })}
            rows={8}
            className="mt-1 w-full rounded border px-2 py-1.5 font-mono text-xs"
            style={inputStyle()} />
        </label>
        <button type="submit" disabled={busy}
          data-testid="agent-db-terraform-upload"
          className="inline-flex w-fit items-center gap-2 rounded px-3 py-2 text-sm"
          style={{ background: 'var(--accent)', color: '#fff' }}>
          <Upload size={15} />
          Upload template
        </button>
      </form>
      <div className="grid gap-2 md:grid-cols-2">
        {templates.map(template => (
          <div key={template.template_id} className="rounded border p-3"
            style={{ borderColor: 'var(--border)' }}>
            <div className="flex items-center justify-between gap-2">
              <span className="text-sm" style={{ color: 'var(--text-primary)' }}>
                {template.template_id}
              </span>
              <span className="text-xs" style={{ color: 'var(--text-secondary)' }}>
                {template.status || 'draft'}
              </span>
            </div>
            <div className="mt-1 text-xs" style={{ color: 'var(--text-secondary)' }}>
              {template.provider}
            </div>
            <Manifest template={template} />
            <div className="mt-3 flex flex-wrap gap-2">
              {template.status === 'draft' && (
                <button type="button" disabled={busy}
                  data-testid="agent-db-terraform-approve"
                  className="rounded border px-2 py-1 text-xs"
                  style={{
                    borderColor: 'var(--border)',
                    color: 'var(--green)',
                  }}
                  onClick={() => onApprove?.(template.template_id)}>
                  Approve
                </button>
              )}
              {template.status === 'approved' && (
                <button type="button" disabled={busy}
                  data-testid="agent-db-terraform-provision"
                  className="rounded border px-2 py-1 text-xs"
                  style={{
                    borderColor: 'var(--border)',
                    color: 'var(--accent)',
                  }}
                  onClick={() => onProvision?.(template)}>
                  Provision
                </button>
              )}
            </div>
          </div>
        ))}
      </div>
    </section>
  )
}

function Manifest({ template }) {
  const files = template.manifest || template.files || []
  if (!files.length) return null
  return (
    <div className="mt-2 text-xs" style={{ color: 'var(--text-secondary)' }}>
      {files.map(file => (typeof file === 'string' ? file : file.path)).join(', ')}
    </div>
  )
}

function TextField({ label, value, onChange, tipKey }) {
  return (
    <label className="block text-xs" style={{ color: 'var(--text-secondary)' }}>
      <FieldTip tipKey={tipKey}>
        <span>{label}</span>
      </FieldTip>
      <input value={value} onChange={e => onChange(e.target.value)}
        className="mt-1 w-full rounded border px-2 py-1.5 text-sm"
        style={inputStyle()} />
    </label>
  )
}

function inputStyle() {
  return {
    background: 'var(--bg-primary)',
    borderColor: 'var(--border)',
    color: 'var(--text-primary)',
  }
}
