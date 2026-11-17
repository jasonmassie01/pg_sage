import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ModelStep } from './ModelStep'
import { stubFetch } from '../../test/fetchStub'

// ux-04b step 2: the Settings model fields (endpoint, model, key), saved
// through the config PUT with the generation it read, then tested through
// POST /api/v1/llm/test with the result beside the field.
// No concurrent access tests: one save-and-test at a time (busy state),
// the generation check is the config API's (useSettingsSave).

afterEach(() => vi.unstubAllGlobals())

const CONFIG = '/api/v1/config/global'
const TEST = 'POST /api/v1/llm/test'

function config({ key = '', readOnly = false } = {}) {
  return {
    mode: 'standalone', desired_generation: 7, ...(readOnly ? { read_only: true } : {}),
    config: {
      'llm.enabled': { value: 'true', source: 'default' },
      'llm.endpoint': { value: 'https://llm.example/v1', source: 'yaml' },
      'llm.model': { value: 'model-a', source: 'yaml' },
      'llm.api_key': { value: key, source: key ? 'yaml' : 'default' },
    },
  }
}

function renderStep(routes, props = {}) {
  const fetchFn = stubFetch({ [`GET ${CONFIG}`]: { json: config() }, ...routes })
  const handlers = { onDone: vi.fn(), onSkip: vi.fn() }
  render(<ModelStep database="app" {...handlers} {...props} />)
  return { fetchFn, ...handlers }
}

const keyField = () => screen.getByTestId('setup-llm-key')
const testBtn = () => screen.getByTestId('setup-llm-test')
const result = () => screen.getByTestId('setup-llm-result')
const type = (el, value) => fireEvent.change(el, { target: { value } })
const calls = (fetchFn, method, url) => fetchFn.mock.calls.filter(
  ([u, init]) => u.split('?')[0] === url && (init?.method || 'GET') === method)

describe('ModelStep', () => {
  it('saves the fields with the read generation, tests the model and finishes', async () => {
    const { fetchFn, onDone } = renderStep({
      [`PUT ${CONFIG}`]: { json: { ok: true } },
      [TEST]: { json: { class: 'ok', latency_ms: 420, model: 'model-b' } },
    })
    await waitFor(() => expect(screen.getByTestId('setup-llm-endpoint'))
      .toHaveValue('https://llm.example/v1'))
    expect(keyField()).toHaveAttribute('type', 'password')
    type(screen.getByTestId('setup-llm-model'), 'model-b')
    type(keyField(), 'sk-test-123')
    fireEvent.click(testBtn())
    await waitFor(() => expect(result()).toHaveTextContent('answered in 420 ms'))
    const [put] = calls(fetchFn, 'PUT', CONFIG)
    expect(JSON.parse(put[1].body)).toEqual({
      'llm.enabled': true, 'llm.endpoint': 'https://llm.example/v1', 'llm.model': 'model-b',
      'llm.api_key': 'sk-test-123', expected_generation: 7,
    })
    expect(calls(fetchFn, 'POST', '/api/v1/llm/test')).toHaveLength(1)
    expect(onDone).toHaveBeenCalledWith('model')
  })

  it('a key of only spaces disables Test, even when a key is stored', async () => {
    const { fetchFn } = renderStep({ [`GET ${CONFIG}`]: { json: config({ key: 'sk-***' }) } })
    await waitFor(() => expect(testBtn()).toBeEnabled())
    type(keyField(), '   ')
    expect(testBtn()).toBeDisabled()
    fireEvent.click(testBtn())
    expect(calls(fetchFn, 'PUT', CONFIG)).toHaveLength(0)
  })

  it('an empty key with no stored key disables Test', async () => {
    renderStep({})
    await waitFor(() => expect(screen.getByTestId('setup-llm-endpoint'))
      .toHaveValue('https://llm.example/v1'))
    expect(testBtn()).toBeDisabled()
  })

  it('a stored key is never sent back: the PUT leaves llm.api_key out', async () => {
    const { fetchFn } = renderStep({
      [`GET ${CONFIG}`]: { json: config({ key: 'sk-***' }) },
      [`PUT ${CONFIG}`]: { json: { ok: true } },
      [TEST]: { json: { class: 'ok', latency_ms: 5 } },
    })
    await waitFor(() => expect(testBtn()).toBeEnabled())
    fireEvent.click(testBtn())
    await waitFor(() => expect(result()).toHaveTextContent('answered'))
    const body = JSON.parse(calls(fetchFn, 'PUT', CONFIG)[0][1].body)
    expect(body).not.toHaveProperty('llm.api_key')
  })

  it.each([
    ['auth', /refused the key/],
    ['rate', /rate limiting/],
    ['timeout', /did not answer in time/],
  ])('a %s probe shows its own message beside the field and does not finish',
    async (cls, want) => {
      const { onDone } = renderStep({
        [`PUT ${CONFIG}`]: { json: { ok: true } },
        [TEST]: { json: { class: cls, error: 'provider said no' } },
      })
      await waitFor(() => expect(screen.getByTestId('setup-llm-endpoint'))
        .toHaveValue('https://llm.example/v1'))
      type(keyField(), 'sk-bad')
      fireEvent.click(testBtn())
      await waitFor(() => expect(result()).toHaveTextContent(want))
      expect(result()).toHaveAttribute('role', 'alert')
      expect(onDone).not.toHaveBeenCalled()
    })

  it('a stale write (409) says another admin saved first and tests nothing', async () => {
    const { fetchFn, onDone } = renderStep({
      [`PUT ${CONFIG}`]: { status: 409, json: { error: 'configuration changed' } },
    })
    await waitFor(() => expect(screen.getByTestId('setup-llm-endpoint'))
      .toHaveValue('https://llm.example/v1'))
    type(keyField(), 'sk-x')
    fireEvent.click(testBtn())
    await waitFor(() => expect(result()).toHaveTextContent(/Another admin/))
    expect(calls(fetchFn, 'POST', '/api/v1/llm/test')).toHaveLength(0)
    expect(onDone).not.toHaveBeenCalled()
  })

  it('a YAML-managed configuration shows the YAML to paste and saves nothing', async () => {
    const { fetchFn } = renderStep({
      [`GET ${CONFIG}`]: { json: config({ readOnly: true }) },
      [TEST]: { json: { class: 'ok', latency_ms: 9 } },
    })
    await waitFor(() => expect(screen.getByTestId('setup-yaml')).toBeInTheDocument())
    type(keyField(), 'sk-yaml')
    expect(screen.getByTestId('setup-yaml')).toHaveTextContent('endpoint: https://llm.example/v1')
    expect(screen.getByTestId('setup-yaml')).not.toHaveTextContent('sk-yaml')
    fireEvent.click(testBtn())
    await waitFor(() => expect(result()).toHaveTextContent('answered'))
    expect(calls(fetchFn, 'PUT', CONFIG)).toHaveLength(0)
  })

  it('Skip says what skipping costs through the wizard', () => {
    const { onSkip } = renderStep({})
    fireEvent.click(screen.getByRole('button', { name: /skip/i }))
    expect(onSkip).toHaveBeenCalledWith('model')
  })
})
