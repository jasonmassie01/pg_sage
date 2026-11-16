import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AutonomyStep } from './AutonomyStep'
import { stubFetch } from '../../test/fetchStub'

// ux-04b step 4: three plain choices mapped onto trust.level and
// execution_mode (decision D6), written through the config PUT with the
// generation read just before; "Do safe things" asks for the typed phrase.

afterEach(() => vi.unstubAllGlobals())

const TRUST = '/api/v1/onboarding/trust?database=orders'
const DB_CONFIG = '/api/v1/config/databases/3'

function guide(grant = {}) {
  return { database: 'orders', current: 'observation', execution_mode: 'approval',
    grant: { method: 'api', key: 'trust.level', config_url: DB_CONFIG,
      execution_mode_writable: true, ...grant } }
}

function renderStep(routes) {
  const fetchFn = stubFetch({
    [`GET ${TRUST}`]: { json: guide() },
    [`GET ${DB_CONFIG}`]: { json: { desired_generation: 11 } },
    ...routes,
  })
  const handlers = { onDone: vi.fn(), onSkip: vi.fn() }
  render(<AutonomyStep database="orders" {...handlers} />)
  return { fetchFn, ...handlers }
}

const puts = fetchFn => fetchFn.mock.calls.filter(([, i]) => i?.method === 'PUT')
const apply = () => fireEvent.click(screen.getByTestId('setup-autonomy-apply'))

describe('AutonomyStep', () => {
  it('offers the three choices, each with a sentence about this database', async () => {
    renderStep({})
    for (const label of ['Watch only', 'Ask me first', 'Do safe things and tell me']) {
      const radio = await screen.findByRole('radio', { name: new RegExp(label) })
      expect(radio).toBeInTheDocument()
    }
    expect(screen.getAllByText(/orders/).length).toBeGreaterThanOrEqual(3)
  })

  it('"Ask me first" writes advisory and approval with the read generation', async () => {
    const { fetchFn, onDone } = renderStep({ [`PUT ${DB_CONFIG}`]: { json: { ok: true } } })
    fireEvent.click(await screen.findByRole('radio', { name: /Ask me first/ }))
    apply()
    await waitFor(() => expect(onDone).toHaveBeenCalledWith('autonomy'))
    expect(JSON.parse(puts(fetchFn)[0][1].body)).toEqual({
      'trust.level': 'advisory', execution_mode: 'approval', expected_generation: 11 })
  })

  it('"Do safe things" needs the typed database name before anything is written',
    async () => {
      const { fetchFn, onDone } = renderStep({ [`PUT ${DB_CONFIG}`]: { json: { ok: true } } })
      fireEvent.click(await screen.findByRole('radio', { name: /Do safe things/ }))
      apply()
      const confirm = await screen.findByTestId('trust-level-confirm-btn')
      expect(confirm).toBeDisabled()
      fireEvent.change(screen.getByTestId('trust-level-phrase'), { target: { value: 'order' } })
      expect(confirm).toBeDisabled()
      expect(puts(fetchFn)).toHaveLength(0)
      fireEvent.change(screen.getByTestId('trust-level-phrase'),
        { target: { value: 'orders' } })
      fireEvent.click(confirm)
      await waitFor(() => expect(onDone).toHaveBeenCalledWith('autonomy'))
      expect(JSON.parse(puts(fetchFn)[0][1].body)).toEqual({
        'trust.level': 'advisory', execution_mode: 'auto', expected_generation: 11 })
    })

  it('cancelling the phrase writes nothing', async () => {
    const { fetchFn, onDone } = renderStep({})
    fireEvent.click(await screen.findByRole('radio', { name: /Do safe things/ }))
    apply()
    fireEvent.click(await screen.findByTestId('trust-level-cancel'))
    expect(puts(fetchFn)).toHaveLength(0)
    expect(onDone).not.toHaveBeenCalled()
  })

  it('a concurrent admin write (409) is refused, said, and the guide is reread',
    async () => {
      const { fetchFn, onDone } = renderStep({
        [`PUT ${DB_CONFIG}`]: { status: 409, json: { error: 'configuration changed' } },
      })
      fireEvent.click(await screen.findByRole('radio', { name: /Watch only/ }))
      apply()
      await waitFor(() => expect(screen.getByTestId('setup-autonomy-error'))
        .toHaveTextContent(/Another admin changed the configuration first/))
      expect(onDone).not.toHaveBeenCalled()
      const trustReads = fetchFn.mock.calls.filter(([u]) => u === TRUST)
      expect(trustReads.length).toBeGreaterThanOrEqual(2)
    })

  it('global scope cannot set execution_mode: it sends trust.level only and says so',
    async () => {
      const { fetchFn, onDone } = renderStep({
        [`GET ${TRUST}`]: { json: guide({ config_url: '/api/v1/config/global',
          execution_mode_writable: false }) },
        'GET /api/v1/config/global': { json: { desired_generation: 4 } },
        'PUT /api/v1/config/global': { json: { ok: true } },
      })
      fireEvent.click(await screen.findByRole('radio', { name: /Ask me first/ }))
      expect(screen.queryByTestId('setup-mode-note')).toBeNull()
      fireEvent.click(screen.getByRole('radio', { name: /Watch only/ }))
      expect(screen.getByTestId('setup-mode-note')).toHaveTextContent('execution_mode: manual')
      apply()
      await waitFor(() => expect(onDone).toHaveBeenCalledWith('autonomy'))
      expect(JSON.parse(puts(fetchFn)[0][1].body)).toEqual({
        'trust.level': 'observation', expected_generation: 4 })
    })

  it('a YAML fleet shows the YAML to paste and writes nothing', async () => {
    const { fetchFn, onDone } = renderStep({
      [`GET ${TRUST}`]: { json: guide({ method: 'yaml', config_url: undefined }) },
    })
    fireEvent.click(await screen.findByRole('radio', { name: /Ask me first/ }))
    expect(screen.getByTestId('setup-yaml')).toHaveTextContent('level: advisory')
    expect(screen.getByTestId('setup-yaml')).toHaveTextContent('execution_mode: approval')
    fireEvent.click(screen.getByTestId('setup-yaml-done'))
    expect(puts(fetchFn)).toHaveLength(0)
    expect(onDone).toHaveBeenCalledWith('autonomy')
  })

  it('a failed guide read says so and offers no choice', async () => {
    renderStep({ [`GET ${TRUST}`]: { status: 500, json: { error: 'read trust guide' } } })
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(/read trust guide/))
    expect(screen.queryByTestId('setup-autonomy-apply')).toBeNull()
  })

  it('Skip keeps pg_sage watching only', () => {
    const { onSkip } = renderStep({})
    fireEvent.click(screen.getByRole('button', { name: /skip/i }))
    expect(onSkip).toHaveBeenCalledWith('autonomy')
  })
})
