import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { WhyNotButton, WhyNotPanel } from './WhyNotPanel'

// ux-08b: the Why not panel shows ux-08a's ten-link chain
// (GET /api/v1/whynot) as a short list, the first failure highlighted and
// one button or copyable statement per fix. One read per open; no shared
// state, so no concurrency test beyond the stale-answer one below.

const apiGet = vi.fn()
vi.mock('../lib/api', () => ({ apiGet: (...args) => apiGet(...args) }))

const NAMES = ['observed', 'finding', 'candidate', 'policy', 'window', 'budget', 'privilege',
  'trust', 'verification_wait', 'emergency_stop']
const GRANT = 'GRANT pg_read_all_stats TO sage;'

function chain(overrides = {}, extra = {}) {
  const links = NAMES.map(name => ({ name, status: 'pass', sentence: `${name} is fine.`,
    first_fail: false, ...overrides[name] }))
  const failing = links.filter(l => l.status === 'fail')
  return { database: 'alpha', links, failing: failing.map(l => l.name),
    first_fail: failing.find(l => l.first_fail)?.name, ...extra }
}

const privilegeFail = () => chain({
  policy: { status: 'fail', sentence: 'Asks first: approval required.',
    fix: { kind: 'ui_link', value: '#/cases?finding=17&database=alpha' } },
  privilege: { status: 'fail', first_fail: true,
    sentence: 'The sage role cannot read other roles’ query text.',
    fix: { kind: 'grant_sql', value: GRANT, note: 'Run it as a superuser.' } },
  trust: { status: 'unknown', sentence: 'The gate stopped at another link.' },
})

let writeText
beforeEach(() => {
  apiGet.mockReset()
  writeText = vi.fn().mockResolvedValue(undefined)
  Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
})
afterEach(() => { vi.useRealTimers() })

const rows = () => screen.getAllByTestId('why-not-link')
const row = name => rows().find(r => r.dataset.link === name)

describe('WhyNotPanel', () => {
  it('lists the ten links, highlights the first failure and copies its GRANT', async () => {
    apiGet.mockResolvedValue(privilegeFail())
    render(<WhyNotPanel database="alpha" findingId="17" />)
    expect(await screen.findAllByTestId('why-not-link')).toHaveLength(10)
    expect(apiGet).toHaveBeenCalledTimes(1)
    expect(apiGet.mock.calls[0][0]).toBe('/api/v1/whynot?database=alpha&finding_id=17')
    const first = rows().filter(r => r.dataset.firstFail === 'true')
    expect(first).toHaveLength(1)
    expect(first[0].dataset.link).toBe('privilege')
    expect(screen.getByTestId('why-not-summary')).toHaveTextContent(
      /^Held back at Privileges: The sage role cannot read/)
    expect(row('privilege')).toHaveTextContent(GRANT)
    expect(row('privilege')).toHaveTextContent('Run it as a superuser.')
    fireEvent.click(within(row('privilege')).getByRole('button', { name: /copy/i }))
    expect(writeText).toHaveBeenCalledWith(GRANT)
    expect(await within(row('privilege')).findByText('Copied')).toBeInTheDocument()
    expect(within(row('policy')).getByRole('link', { name: /open/i }))
      .toHaveAttribute('href', '#/cases?finding=17&database=alpha')
  })

  it('renders a fix kind it does not know as its sentence, with no button', async () => {
    apiGet.mockResolvedValue(chain({ window: { status: 'fail', first_fail: true,
      sentence: 'Outside the maintenance window.',
      fix: { kind: 'teleport', value: '<img src=x>' } } }))
    const { container } = render(<WhyNotPanel database="alpha" />)
    await screen.findAllByTestId('why-not-link')
    expect(row('window')).toHaveTextContent('Outside the maintenance window.')
    expect(within(row('window')).queryByRole('button')).toBeNull()
    expect(within(row('window')).queryByRole('link')).toBeNull()
    expect(row('window')).not.toHaveTextContent('teleport')
    expect(container.querySelector('img')).toBeNull()
  })

  it('says nothing holds back an all-pass chain', async () => {
    apiGet.mockResolvedValue(chain({ finding: { status: 'not_applicable',
      sentence: 'No finding was asked about.' } }))
    render(<WhyNotPanel database="alpha" />)
    expect(await screen.findByTestId('why-not-summary')).toHaveTextContent(
      "Nothing is holding this back; it will run at the executor's next pass.")
    expect(rows().filter(r => r.dataset.firstFail === 'true')).toHaveLength(0)
  })

  it('never says nothing holds back a chain with an unknown link', async () => {
    apiGet.mockResolvedValue(chain({ privilege: { status: 'unknown',
      sentence: 'The privileges could not be read.', error: 'timeout' } }))
    render(<WhyNotPanel database="alpha" />)
    const summary = await screen.findByTestId('why-not-summary')
    expect(summary).toHaveTextContent('Nothing failed, but 1 check could not be made')
    expect(summary).not.toHaveTextContent('Nothing is holding this back')
  })

  it('styles an unknown link as unknown, never as pass', async () => {
    apiGet.mockResolvedValue(privilegeFail())
    render(<WhyNotPanel database="alpha" />)
    await screen.findAllByTestId('why-not-link')
    const unknown = row('trust')
    const pass = row('observed')
    expect(unknown.dataset.status).toBe('unknown')
    expect(within(unknown).getByTestId('why-not-status')).toHaveTextContent('Unknown')
    expect(within(pass).getByTestId('why-not-status')).toHaveTextContent('Pass')
    expect(within(unknown).getByTestId('why-not-status').getAttribute('style'))
      .not.toBe(within(pass).getByTestId('why-not-status').getAttribute('style'))
  })

  it('shows the error banner on a failed read and no chain', async () => {
    apiGet.mockResolvedValueOnce(privilegeFail())
    const { rerender } = render(<WhyNotPanel database="alpha" findingId="17" />)
    await screen.findAllByTestId('why-not-link')
    apiGet.mockRejectedValueOnce(new Error('internal error (500)'))
    rerender(<WhyNotPanel database="alpha" findingId="18" />)
    expect(await screen.findByRole('alert')).toHaveTextContent('internal error (500)')
    expect(screen.queryAllByTestId('why-not-link')).toHaveLength(0)
    expect(screen.queryByTestId('why-not-summary')).toBeNull()
  })

  it('shows B after A, and drops a late answer for A', async () => {
    let answerA
    apiGet.mockImplementationOnce(() => new Promise(resolve => { answerA = resolve }))
    const { rerender } = render(<WhyNotPanel database="alpha" findingId="1" />)
    apiGet.mockResolvedValueOnce(chain({ budget: { status: 'fail', first_fail: true,
      sentence: 'The change budget for this window is spent.' } }, { database: 'beta' }))
    rerender(<WhyNotPanel database="beta" findingId="2" />)
    await screen.findAllByTestId('why-not-link')
    await act(async () => { answerA(privilegeFail()) })
    expect(rows().filter(r => r.dataset.firstFail === 'true')[0].dataset.link).toBe('budget')
    expect(screen.queryByText(GRANT)).toBeNull()
    expect(apiGet.mock.calls.map(c => c[0])).toEqual([
      '/api/v1/whynot?database=alpha&finding_id=1',
      '/api/v1/whynot?database=beta&finding_id=2'])
  })

  it('asks by object or category and reads nothing without one database', async () => {
    apiGet.mockResolvedValue(chain())
    const { rerender } = render(<WhyNotPanel database="alpha" object="public.orders"
      category="unused_index" />)
    await screen.findAllByTestId('why-not-link')
    expect(apiGet.mock.calls[0][0]).toBe(
      '/api/v1/whynot?database=alpha&object=public.orders&category=unused_index')
    rerender(<WhyNotPanel database="all" />)
    expect(screen.getByTestId('why-not-pick-database')).toBeInTheDocument()
    expect(apiGet).toHaveBeenCalledTimes(1)
  })

  it('offers a setting as a copyable statement and a wait as its time', async () => {
    apiGet.mockResolvedValue(chain({
      finding: { status: 'fail', first_fail: true, sentence: 'LLM is off.',
        fix: { kind: 'setting', value: 'llm.enabled=true' } },
      budget: { status: 'fail', sentence: 'The change budget for this window is spent.',
        fix: { kind: 'wait_until', value: '2026-11-18T18:00:00Z' } } }))
    render(<WhyNotPanel database="alpha" />)
    await screen.findAllByTestId('why-not-link')
    expect(row('finding')).toHaveTextContent('llm.enabled=true')
    expect(within(row('finding')).getByRole('link', { name: /settings/i }))
      .toHaveAttribute('href', '#/settings')
    fireEvent.click(within(row('finding')).getByRole('button', { name: /copy/i }))
    expect(writeText).toHaveBeenCalledWith('llm.enabled=true')
    expect(within(row('budget')).getByTestId('why-not-wait')).toHaveTextContent(/^Clears /)
    expect(within(row('budget')).queryByRole('button')).toBeNull()
  })
})

describe('WhyNotButton', () => {
  it('opens the panel on click and closes it again', async () => {
    apiGet.mockResolvedValue(chain())
    render(<WhyNotButton database="alpha" findingId="3" user={{ role: 'operator' }} />)
    expect(apiGet).not.toHaveBeenCalled()
    const button = screen.getByRole('button', { name: 'Why not?' })
    expect(button).toHaveAttribute('aria-expanded', 'false')
    fireEvent.click(button)
    expect(button).toHaveAttribute('aria-expanded', 'true')
    expect(await screen.findAllByTestId('why-not-link')).toHaveLength(10)
    fireEvent.click(button)
    expect(screen.queryByTestId('why-not-panel')).toBeNull()
  })

  it('is not offered to a viewer, whom the route refuses', () => {
    const { container } = render(<WhyNotButton database="alpha" user={{ role: 'viewer' }} />)
    expect(container).toBeEmptyDOMElement()
  })
})
