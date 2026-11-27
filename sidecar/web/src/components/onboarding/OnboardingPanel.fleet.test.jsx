import { fireEvent, render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { OnboardingPanel } from './OnboardingPanel'
import { FLEET_SUMMARY_FROM } from '../../lib/fleetScope'

// ux-10b: in the "all" scope of a fleet of FLEET_SUMMARY_FROM or more, the
// setup checklist is "N of M set up" with the incomplete databases listed
// and paged, not one checklist per database. No concurrent access tests:
// a pure render of one API answer.

const api = vi.hoisted(() => ({ useAPI: vi.fn() }))

vi.mock('../../hooks/useAPI', () => ({
  useAPI: (...args) => api.useAPI(...args),
}))

const name = i => `db-${String(i).padStart(3, '0')}`

function entryOf(i, { done = true, error } = {}) {
  return {
    database: name(i), connected: true, trust_level: 'observation', install_kind: 'new',
    ...(error ? { error } : {}),
    steps: [
      { id: 'connected', label: 'Connected', done: true },
      { id: 'extensions', label: 'Extensions', done },
      { id: 'mcp_token', label: 'MCP token', done: false, optional: true },
    ],
  }
}

function respond(entries) {
  api.useAPI.mockImplementation(() => ({
    data: { databases: entries }, loading: false, error: null, refetch: vi.fn(),
  }))
}

const panel = () => screen.getByTestId('onboarding-summary')
const rows = () => within(panel()).queryAllByTestId('onboarding-incomplete-row')

describe('OnboardingPanel fleet summary', () => {
  beforeEach(() => api.useAPI.mockReset())

  it('says how many of 500 are set up and pages the incomplete ones', () => {
    respond(Array.from({ length: 500 }, (_, i) => entryOf(i, { done: i % 25 !== 0 })))
    render(<OnboardingPanel database="all" />)
    expect(panel()).toHaveTextContent('480 of 500 databases set up.')
    expect(rows()).toHaveLength(10)
    expect(rows()[0]).toHaveTextContent(`${name(0)}Next: Extensions`)
    expect(document.querySelectorAll('[data-db-block]')).toHaveLength(0)
    expect(screen.queryByRole('list', { name: 'Setup checklist' })).toBeNull()
    fireEvent.click(within(panel()).getByRole('button', { name: 'Show 10 more' }))
    expect(rows()).toHaveLength(20)
    expect(rows().at(-1).dataset.database).toBe(name(475))
    expect(within(panel()).queryByRole('button', { name: /more/ })).toBeNull()
  })

  it('lists a database pg_sage could not read as incomplete', () => {
    respond(Array.from({ length: 8 }, (_, i) =>
      entryOf(i, { error: i === 3 ? 'permission denied' : undefined })))
    render(<OnboardingPanel database="all" />)
    expect(panel()).toHaveTextContent('7 of 8 databases set up.')
    expect(rows().map(r => r.dataset.database)).toEqual([name(3)])
    expect(rows()[0]).toHaveTextContent('Could not read')
  })

  it('opens an incomplete database through the scope', () => {
    const onSelectDatabase = vi.fn()
    respond(Array.from({ length: 8 }, (_, i) => entryOf(i, { done: i !== 6 })))
    render(<OnboardingPanel database="all" onSelectDatabase={onSelectDatabase} />)
    fireEvent.click(within(rows()[0]).getByRole('button', { name: name(6) }))
    expect(onSelectDatabase).toHaveBeenCalledWith(name(6))
  })

  it('stops polling once the whole fleet is set up', () => {
    respond(Array.from({ length: 500 }, (_, i) => entryOf(i)))
    render(<OnboardingPanel database="all" />)
    expect(panel()).toHaveTextContent('500 of 500 databases set up.')
    expect(panel()).toHaveTextContent('Setup complete.')
    expect(rows()).toHaveLength(0)
    expect(api.useAPI.mock.calls.at(-1)[1]).toBe(0)
  })

  it('keeps one checklist per database below the threshold', () => {
    respond(Array.from({ length: FLEET_SUMMARY_FROM - 1 }, (_, i) =>
      entryOf(i, { done: false })))
    render(<OnboardingPanel database="all" />)
    expect(screen.queryByTestId('onboarding-summary')).toBeNull()
    expect(screen.getAllByRole('list', { name: 'Setup checklist' }))
      .toHaveLength(FLEET_SUMMARY_FROM - 1)
  })

  it('summarizes from exactly the threshold', () => {
    respond(Array.from({ length: FLEET_SUMMARY_FROM }, (_, i) => entryOf(i, { done: i > 0 })))
    render(<OnboardingPanel database="all" />)
    expect(panel()).toHaveTextContent(
      `${FLEET_SUMMARY_FROM - 1} of ${FLEET_SUMMARY_FROM} databases set up.`)
    expect(rows()).toHaveLength(1)
  })
})
