import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { SetupWizard } from './SetupWizard'
import { stubFetch } from '../../test/fetchStub'

// ux-04b: the four-step wizard. The steps are stubbed here (each has its
// own test file); this file pins the stepper: status from the checklist,
// skips and what they cost, the recorded first-done times, and the
// landing on the first look.

vi.mock('./ConnectStep', () => ({ ConnectStep: stub('connect') }))
vi.mock('./ModelStep', () => ({ ModelStep: stub('model') }))
vi.mock('./ChannelStep', () => ({ ChannelStep: stub('channel') }))
vi.mock('./AutonomyStep', () => ({ AutonomyStep: stub('autonomy') }))

function stub(key) {
  return function Step({ onDone, onSkip }) {
    return <div data-testid={`step-body-${key}`}>
      <button type="button" onClick={() => onDone(key)}>finish {key}</button>
      <button type="button" onClick={() => onSkip(key)}>skip {key}</button>
    </div>
  }
}

const SETUP = 'POST /api/v1/onboarding/setup'
const steps = (done = {}) => ['connected', 'extensions', 'first_look', 'model', 'mcp_token',
  'notifications', 'grant_more'].map(id => ({ id, done: Boolean(done[id]) }))
const entry = done => ({ database: 'app', install_kind: 'new', steps: steps(done), setup: {} })
const nav = key => screen.getByTestId(`setup-step-${key}`)

function renderWizard(props = {}, routes = { [SETUP]: { json: { recorded: true } } }) {
  const fetchFn = stubFetch(routes)
  const handlers = { onChanged: vi.fn(), onFinished: vi.fn() }
  const view = render(<SetupWizard entry={entry({ connected: true })} {...handlers}
    {...props} />)
  return { fetchFn, view, ...handlers }
}

beforeEach(() => window.localStorage.clear())
afterEach(() => vi.unstubAllGlobals())

describe('SetupWizard', () => {
  it('shows four steps with their checklist status and opens the first one not done', () => {
    renderWizard()
    expect(screen.getByRole('heading', { name: 'Set up pg_sage' })).toBeInTheDocument()
    expect(['connect', 'model', 'channel', 'autonomy'].map(k => nav(k).dataset.status))
      .toEqual(['done', 'todo', 'todo', 'todo'])
    expect(nav('model')).toHaveAttribute('aria-current', 'step')
    expect(screen.getByTestId('step-body-model')).toBeInTheDocument()
  })

  it('skipping Model says what it costs, leaves the checklist step undone and moves on',
    () => {
      const { fetchFn, onChanged } = renderWizard()
      fireEvent.click(screen.getByText('skip model'))
      expect(nav('model').dataset.status).toBe('skipped')
      expect(screen.getByTestId('setup-skip-note-model'))
        .toHaveTextContent('Skipped: LLM features stay off until a model is set')
      expect(screen.getByTestId('step-body-channel')).toBeInTheDocument()
      expect(fetchFn).not.toHaveBeenCalled()
      expect(onChanged).not.toHaveBeenCalled()
      expect(JSON.parse(window.localStorage.getItem('pg_sage.setup.skipped.app')))
        .toEqual(['model'])
    })

  it('finishing a step records its first-done time and rereads the checklist', async () => {
    const { fetchFn, onChanged } = renderWizard()
    fireEvent.click(screen.getByText('finish model'))
    await waitFor(() => expect(onChanged).toHaveBeenCalled())
    const [url, init] = fetchFn.mock.calls[0]
    expect(`${init.method} ${url}`).toBe(SETUP)
    expect(JSON.parse(init.body)).toEqual({ database: 'app', step: 'model' })
    // Review fix: the step stays open so its result stays readable, until Next.
    expect(screen.getByTestId('step-body-model')).toBeInTheDocument()
    fireEvent.click(await screen.findByTestId('setup-step-next'))
    expect(screen.getByTestId('step-body-channel')).toBeInTheDocument()
    expect(screen.queryByTestId('setup-step-next')).toBeNull()
  })

  it('finishing a step that was already done moves on at once', async () => {
    const { fetchFn } = renderWizard({ entry: entry({ connected: true, notifications: true }) })
    fireEvent.click(nav('channel'))
    fireEvent.click(screen.getByText('finish channel'))
    expect(await screen.findByTestId('step-body-autonomy')).toBeInTheDocument()
    expect(fetchFn).toHaveBeenCalledTimes(1)
    expect(screen.queryByTestId('setup-step-next')).toBeNull()
  })

  it('a step left through the step list does not keep its Next', async () => {
    const { onChanged } = renderWizard()
    fireEvent.click(screen.getByText('finish model'))
    await waitFor(() => expect(onChanged).toHaveBeenCalled())
    expect(await screen.findByTestId('setup-step-next')).toBeInTheDocument()
    fireEvent.click(nav('channel'))
    fireEvent.click(nav('model'))
    expect(screen.queryByTestId('setup-step-next')).toBeNull()
  })

  it('a refused record (409) is shown and the step stays open', async () => {
    const { onChanged } = renderWizard({}, {
      [SETUP]: { status: 409, json: { error: 'the model has not answered yet' } } })
    fireEvent.click(screen.getByText('finish model'))
    expect(await screen.findByTestId('setup-record-error'))
      .toHaveTextContent('the model has not answered yet')
    expect(screen.getByTestId('step-body-model')).toBeInTheDocument()
    expect(onChanged).toHaveBeenCalled()
  })

  it('a connect step records nothing: the checklist reads the connection itself', async () => {
    const { fetchFn, onChanged } = renderWizard({ entry: null })
    fireEvent.click(screen.getByText('finish connect'))
    await waitFor(() => expect(onChanged).toHaveBeenCalled())
    expect(fetchFn).not.toHaveBeenCalled()
  })

  it('with no database yet only Connect can open', () => {
    renderWizard({ entry: null })
    expect(screen.getByTestId('step-body-connect')).toBeInTheDocument()
    for (const k of ['model', 'channel', 'autonomy']) expect(nav(k)).toBeDisabled()
  })

  it('moves through every state to done and lands on the first look', async () => {
    const { view, onFinished } = renderWizard()
    const rerender = done => view.rerender(<SetupWizard entry={entry(done)}
      onChanged={vi.fn()} onFinished={onFinished} />)
    rerender({ connected: true, model: true })
    expect(nav('model').dataset.status).toBe('done')
    rerender({ connected: true, model: true, notifications: true })
    expect(nav('channel').dataset.status).toBe('done')
    expect(onFinished).not.toHaveBeenCalled()
    rerender({ connected: true, model: true, notifications: true, grant_more: true })
    expect(await screen.findByTestId('setup-finished')).toHaveTextContent(/first look/i)
    expect(onFinished).toHaveBeenCalledTimes(1)
  })

  it('a channel deleted later moves step 3 back to not done', () => {
    const { view } = renderWizard({ entry: entry({ connected: true, notifications: true }) })
    expect(nav('channel').dataset.status).toBe('done')
    view.rerender(<SetupWizard entry={entry({ connected: true })} onChanged={vi.fn()}
      onFinished={vi.fn()} />)
    expect(nav('channel').dataset.status).toBe('todo')
  })

  it('reopens a done step from the step list', () => {
    renderWizard()
    fireEvent.click(nav('connect'))
    expect(screen.getByTestId('step-body-connect')).toBeInTheDocument()
    expect(nav('connect')).toHaveAttribute('aria-current', 'step')
  })
})
