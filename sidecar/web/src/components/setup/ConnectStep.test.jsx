import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ConnectStep } from './ConnectStep'
import { stubFetch } from '../../test/fetchStub'

// ux-04b step 1: paste a URL (parsed in the browser into DatabaseForm),
// the provider's role SQL, then the doctor report once connected.
// No concurrent access tests: one form, one save per click.

afterEach(() => vi.unstubAllGlobals())

const CONFIG = { json: { mode: 'fleet', desired_generation: 1, config: {} } }
const RDS = 'postgres://sage_agent:s3cret%21@orders.c9.us-east-1.rds.amazonaws.com:5432/' +
  'orders?sslmode=verify-full'

function renderStep(routes = {}, props = {}) {
  const fetchFn = stubFetch({ 'GET /api/v1/config/global': CONFIG, ...routes })
  const handlers = { onDone: vi.fn(), onSkip: vi.fn() }
  render(<ConnectStep database={null} connected={false} {...handlers} {...props} />)
  return { fetchFn, ...handlers }
}

function paste(text) {
  fireEvent.change(screen.getByTestId('setup-url'), { target: { value: text } })
  fireEvent.click(screen.getByTestId('setup-url-parse'))
}

const writes = fetchFn => fetchFn.mock.calls.filter(([, i]) => i?.method && i.method !== 'GET')

describe('ConnectStep: paste a URL', () => {
  it('fills the form, masks the password, detects RDS and shows its role SQL', async () => {
    renderStep()
    paste(RDS)
    const form = await screen.findByTestId('db-form')
    expect(within(form).getByTestId('db-host'))
      .toHaveValue('orders.c9.us-east-1.rds.amazonaws.com')
    expect(within(form).getByTestId('db-database')).toHaveValue('orders')
    expect(within(form).getByTestId('db-username')).toHaveValue('sage_agent')
    expect(within(form).getByTestId('db-sslmode')).toHaveValue('verify-full')
    const password = within(form).getByTestId('db-password')
    expect(password).toHaveAttribute('type', 'password')
    expect(password).toHaveValue('s3cret!')
    expect(screen.getByTestId('setup-url')).not.toHaveValue(expect.stringContaining('s3cret'))
    expect(screen.getByTestId('setup-url').value).toContain(':****@')
    expect(screen.getByTestId('setup-provider')).toHaveValue('rds')
    expect(screen.getByTestId('setup-role-sql')).toHaveTextContent('rds_superuser')
    expect(screen.getByTestId('setup-role-sql'))
      .toHaveTextContent('GRANT pg_monitor TO sage_agent;')
    expect(document.body.textContent).not.toContain('s3cret')
  })

  it('a chosen provider preset changes the role SQL', async () => {
    renderStep()
    paste('postgres://sage_agent:pw@10.0.0.5/app')
    expect(await screen.findByTestId('setup-provider')).toHaveValue('unknown')
    fireEvent.change(screen.getByTestId('setup-provider'), { target: { value: 'cloud-sql' } })
    expect(screen.getByTestId('setup-role-sql')).toHaveTextContent('cloudsqlsuperuser')
  })

  it.each([
    ['postgres:///orders', 'no host'],
    ['mysql://u:p@h/orders', 'Unknown scheme'],
    ['postgres://u:p@h/orders?sslmode=bogus', 'sslmode "bogus"'],
  ])('%s shows a field error and saves nothing', async (url, want) => {
    const { fetchFn } = renderStep()
    paste(url)
    expect(await screen.findByTestId('setup-url-error')).toHaveTextContent(want)
    expect(screen.getByTestId('setup-url-error')).toHaveAttribute('role', 'alert')
    expect(screen.queryByTestId('db-form')).toBeNull()
    expect(writes(fetchFn)).toHaveLength(0)
  })

  it('saving the form adds the database and finishes the step', async () => {
    const { fetchFn, onDone } = renderStep({
      'POST /api/v1/databases/managed': { status: 201, json: { id: 3, name: 'orders' } },
    })
    paste(RDS)
    fireEvent.click(await screen.findByTestId('db-save-button'))
    await waitFor(() => expect(onDone).toHaveBeenCalledWith('connect'))
    const body = JSON.parse(writes(fetchFn)[0][1].body)
    expect(body).toMatchObject({ name: 'orders', host: 'orders.c9.us-east-1.rds.amazonaws.com',
      port: 5432, database_name: 'orders', username: 'sage_agent', password: 's3cret!',
      sslmode: 'verify-full', trust_level: 'observation' })
  })

  it('Lakebase shows the Databricks OAuth path instead of a password form', async () => {
    renderStep()
    paste('postgres://sp-role@inst.database.us-east-1.cloud.databricks.com/app' +
      '?sslmode=verify-full')
    expect(await screen.findByTestId('setup-oauth-note')).toHaveTextContent(/OAuth/)
    expect(screen.queryByTestId('db-password')).toBeNull()
    expect(screen.getByTestId('setup-role-sql')).not.toHaveTextContent('PASSWORD')
  })

  it('a YAML fleet shows the YAML to paste instead of saving', async () => {
    const { fetchFn } = renderStep({
      'GET /api/v1/config/global': { json: { mode: 'fleet', read_only: true, config: {} } },
    })
    await waitFor(() => expect(fetchFn).toHaveBeenCalled())
    paste(RDS)
    const yaml = await screen.findByTestId('setup-yaml')
    expect(yaml).toHaveTextContent('host: orders.c9.us-east-1.rds.amazonaws.com')
    expect(yaml).toHaveTextContent('sslmode: verify-full')
    expect(yaml).not.toHaveTextContent('s3cret')
    expect(screen.queryByTestId('db-form')).toBeNull()
  })
})

describe('ConnectStep: connected', () => {
  const report = checks => ({ json: { provider: 'rds', checks,
    counts: { pass: 1, warn: 0, fail: 0, unknown: 0 } } })
  const DOCTOR = 'GET /api/v1/doctor?database=orders'

  it('shows the doctor report; warnings do not block Next', async () => {
    const { onDone } = renderStep({ [DOCTOR]: report([
      { id: 'connection', title: 'Connection', status: 'pass', detail: 'connected' },
      { id: 'extensions', title: 'Extensions', status: 'warn', detail: 'hypopg missing',
        fix: 'CREATE EXTENSION hypopg;' },
      { id: 'role_grants', title: 'Role', status: 'fail', detail: 'no pg_monitor',
        fix: 'GRANT pg_monitor TO sage_agent' },
    ]) }, { database: 'orders', connected: true })
    expect(await screen.findByTestId('doctor-check-extensions'))
      .toHaveAttribute('data-status', 'warn')
    expect(screen.getByTestId('doctor-check-role_grants')).toHaveTextContent('GRANT pg_monitor')
    expect(screen.getByTestId('setup-next')).toBeEnabled()
    fireEvent.click(screen.getByTestId('setup-next'))
    expect(onDone).toHaveBeenCalledWith('connect')
  })

  it('a failed connection check blocks Next', async () => {
    const { onDone } = renderStep({ [DOCTOR]: report([
      { id: 'connection', title: 'Connection', status: 'fail', detail: 'refused' },
    ]) }, { database: 'orders', connected: true })
    expect(await screen.findByTestId('doctor-check-connection'))
      .toHaveAttribute('data-status', 'fail')
    expect(screen.getByTestId('setup-next')).toBeDisabled()
    expect(screen.getByText(/Fix the connection/)).toBeInTheDocument()
    fireEvent.click(screen.getByTestId('setup-next'))
    expect(onDone).not.toHaveBeenCalled()
  })

  it('a doctor read failure is named and does not block a connected database', async () => {
    renderStep({ [DOCTOR]: { status: 500, json: { error: 'doctor failed' } } },
      { database: 'orders', connected: true })
    expect(await screen.findByText(/doctor failed/)).toBeInTheDocument()
    expect(screen.getByTestId('setup-next')).toBeEnabled()
  })
})
