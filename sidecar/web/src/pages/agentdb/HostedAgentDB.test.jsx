import { useState } from 'react'
import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ProvisionForm } from './AgentDBSections'
import { ProviderReadinessPanel } from './AgentDBProvisioningPanels'
import { ProviderSettingsPanel } from './ProviderSettingsPanel'
import { hostedMetadata } from './hostedMetadata'
import { hostedSecretReference } from './hostedMetadata'
import { BlueprintBuilderPanel } from './BlueprintBuilderPanel'
import { TerraformTemplatePanel } from './TerraformTemplatePanel'

const profiles = ['neon', 'supabase'].flatMap(provider => ['branch', 'project'].map(mode => ({
  profile_id: `${provider}_${mode}`, provider, provisioning_level: 'instance',
  name: `${provider} ${mode}`, provider_params: { mode },
})))

function FormHarness() {
  const [form, setForm] = useState({ provider: 'local_postgres', provisioning_level: 'schema',
    size_profile_id: 'local_schema_xs', workload_types: [], extensions: [] })
  return <ProvisionForm form={form} profiles={profiles} onChange={setForm} onSubmit={vi.fn()} />
}

describe('Hosted AgentDB support', () => {
  it.each(['neon', 'supabase'])('selects %s profiles without retaining local size', provider => {
    render(<FormHarness />)
    fireEvent.change(screen.getByLabelText('Provider'), { target: { value: provider } })
    expect(screen.getByLabelText('Provider')).toHaveValue(provider)
    expect(screen.getByLabelText('Level')).toHaveValue('instance')
    expect(screen.getByLabelText('Size')).toHaveValue(`${provider}_branch`)
    expect(screen.getByRole('option', { name: `${provider} project` })).toBeInTheDocument()
    fireEvent.change(screen.getByLabelText('Size'), { target: { value: `${provider}_project` } })
    fireEvent.change(screen.getByLabelText('Organization'), { target: { value: 'owned-org' } })
    expect(screen.getByLabelText('Organization')).toHaveValue('owned-org')
    fireEvent.change(screen.getByLabelText('Environment DSN reference'),
      { target: { value: 'env:SAGE_OWNED_DATABASE_URL' } })
    expect(screen.getByLabelText('Environment DSN reference'))
      .toHaveValue('env:SAGE_OWNED_DATABASE_URL')
    for (const label of ['Parent project', 'Source branch', 'Region']) {
      fireEvent.change(screen.getByLabelText(label), { target: { value: 'owned-value' } })
      expect(screen.getByLabelText(label)).toHaveValue('owned-value')
    }
  })

  it('maps exact hosted scopes without fabricating mode or entitlement', () => {
    expect(hostedMetadata({ cloud_project: 'owned-project', cloud_organization: 'owned-org',
      hosted_source_branch: 'parent-branch', cloud_region: 'us-east-1' })).toEqual({
      project: 'owned-project', organization: 'owned-org', source_branch: 'parent-branch',
      region: 'us-east-1',
    })
    expect(hostedMetadata({})).toEqual({})
  })

  it('preserves runtime missing and unknown readiness', () => {
    render(<ProviderReadinessPanel providers={[
      { provider: 'supabase', label: 'Supabase', found: false, detail: 'Runner disabled by policy' },
      { provider: 'neon', label: 'Neon' },
    ]} />)
    expect(screen.getByText('missing')).toBeInTheDocument()
    expect(screen.getByText('unknown')).toBeInTheDocument()
    expect(screen.getByText('Runner disabled by policy')).toBeInTheDocument()
    expect(screen.queryByText('ready')).not.toBeInTheDocument()
  })

  it.each(['neon', 'supabase'])('makes %s policy settings selectable', provider => {
    render(<ProviderSettingsPanel configs={[]} providers={[]} onSave={vi.fn()} />)
    fireEvent.change(screen.getByLabelText('Settings provider'), { target: { value: provider } })
    expect(screen.getByLabelText('Settings provider')).toHaveValue(provider)
  })

  it('accepts only optional environment DSN references', () => {
    expect(hostedSecretReference({})).toEqual({})
    expect(hostedSecretReference({ secret_ref: 'env:SAGE_OWNED_DSN' })).toEqual({
      secret_ref: 'env:SAGE_OWNED_DSN', secret_ref_provider: 'env',
    })
    expect(hostedSecretReference({ secret_ref: 'env://SAGE_OWNED_DSN' })).toEqual({
      secret_ref: 'env://SAGE_OWNED_DSN', secret_ref_provider: 'env',
    })
    for (const secret_ref of ['postgres://user:password@host/db', 'env:1INVALID', 42]) {
      expect(() => hostedSecretReference({ secret_ref })).toThrow('environment')
    }
  })

  it.each(['neon', 'supabase'])('submits %s blueprint intent and template identity', provider => {
    const generate = vi.fn()
    const upload = vi.fn()
    render(<><BlueprintBuilderPanel blueprints={[]} onGenerate={generate} />
      <TerraformTemplatePanel templates={[]} onUpload={upload} /></>)
    fireEvent.change(screen.getByLabelText('Blueprint provider'), { target: { value: provider } })
    fireEvent.change(screen.getByLabelText('Intent'), { target: { value: 'isolated branch' } })
    fireEvent.click(screen.getByTestId('agent-db-blueprint-submit'))
    expect(generate).toHaveBeenCalledWith(expect.objectContaining({
      provider, intent: 'isolated branch',
    }))
    fireEvent.change(screen.getByLabelText('Template provider'), { target: { value: provider } })
    fireEvent.change(screen.getByLabelText('Terraform content'), { target: { value: 'resource {}' } })
    fireEvent.click(screen.getByTestId('agent-db-terraform-upload'))
    expect(upload).toHaveBeenCalledWith(expect.objectContaining({
      provider, files: [{ path: 'main.tf', body: 'resource {}' }],
    }))
  })
})

// No concurrent-access tests: these React controls have no shared mutable service state.
