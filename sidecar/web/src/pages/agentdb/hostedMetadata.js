export function hostedMetadata(form) {
  return Object.fromEntries([
    ['project', form.cloud_project], ['organization', form.cloud_organization],
    ['source_branch', form.hosted_source_branch], ['region', form.cloud_region],
  ].filter(([, value]) => typeof value === 'string' && value.trim() !== ''))
}

export function hostedSecretReference(form) {
  const reference = form.secret_ref
  if (reference === undefined || reference === '') return {}
  if (typeof reference !== 'string' || !/^env:(\/\/)?[A-Za-z_][A-Za-z0-9_]*$/.test(reference)) {
    throw new Error('Use an environment reference such as env:SAGE_DATABASE_URL')
  }
  return { secret_ref: reference, secret_ref_provider: 'env' }
}
