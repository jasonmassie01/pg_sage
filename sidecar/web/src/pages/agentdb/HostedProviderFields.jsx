import { TextField } from './AgentDBFormControls'

export function HostedProviderFields({ form, update }) {
  return <div className="grid gap-3 sm:grid-cols-2">
    <TextField label="Parent project" value={form.cloud_project || ''}
      onChange={value => update('cloud_project', value)} />
    <TextField label="Organization" value={form.cloud_organization || ''}
      onChange={value => update('cloud_organization', value)} />
    <TextField label="Source branch" value={form.hosted_source_branch || ''}
      onChange={value => update('hosted_source_branch', value)} />
    <TextField label="Region" value={form.cloud_region || ''}
      onChange={value => update('cloud_region', value)} />
    <TextField label="Environment DSN reference" value={form.secret_ref || ''}
      onChange={value => update('secret_ref', value)} />
    <p className="text-xs sm:col-span-2" style={{ color: 'var(--text-secondary)' }}>
      Branch profiles require a parent project; project profiles require an organization.
      Availability and quotas are checked during preflight.
      Optional connection references use env:VARIABLE or env://VARIABLE;
      the sidecar resolves the DSN from its environment.
    </p>
  </div>
}
