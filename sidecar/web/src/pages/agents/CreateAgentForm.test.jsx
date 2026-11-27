import { fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { CreateAgentForm } from './CreateAgentForm'
import { bodiesOf, coder, response, stubAPI, users } from './testStub'

// agentgov-06: the profile is picked from the profiles the server ships
// (GET /api/v1/agents metadata), never typed, so a typo cannot create an
// agent whose every agent_* call is refused.

const el = id => screen.getByTestId(id)
const type = (id, value) => fireEvent.change(el(id), { target: { value } })

const profiles = [
  { name: 'app-writer', classes: ['read', 'write_insert', 'write_update'],
    env_ceiling: 'prod', direct_lane: true },
  { name: 'coding-agent', classes: ['read', 'ddl_additive'], env_ceiling: 'stage',
    direct_lane: false },
  { name: 'legacy', classes: ['read'], env_ceiling: 'prod', direct_lane: false },
  { name: 'readonly-analyst', classes: ['read'], env_ceiling: 'prod', direct_lane: false },
]

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('CreateAgentForm profile', () => {
  it('is a select of the served profiles, defaulting to readonly-analyst', () => {
    stubAPI()
    render(<CreateAgentForm users={users} profiles={profiles} />)
    const field = el('agent-create-profile')
    expect(field.tagName).toBe('SELECT')
    expect(field.value).toBe('readonly-analyst')
    const names = within(field).getAllByRole('option').map(o => o.value)
    expect(names).toEqual(['app-writer', 'coding-agent', 'legacy', 'readonly-analyst'])
    expect(within(field).getByRole('option', { name: /coding-agent/ }).textContent)
      .toContain('stage')
  })

  it('sends the chosen profile', async () => {
    const mock = stubAPI({
      'POST /api/v1/agents': () => response(201, { ...coder, name: 'ci-bot' }),
    })
    const onCreated = vi.fn()
    render(<CreateAgentForm users={users} profiles={profiles} onCreated={onCreated} />)
    type('agent-create-name', 'ci-bot')
    type('agent-create-sponsor', '5')
    type('agent-create-profile', 'coding-agent')
    fireEvent.click(el('agent-create-submit'))
    await screen.findByTestId('agent-create-done')
    expect(bodiesOf(mock, 'POST', '/api/v1/agents')).toEqual([{ name: 'ci-bot',
      sponsor_user_id: 5, profile: 'coding-agent', env_ceiling: 'dev' }])
    expect(onCreated).toHaveBeenCalledTimes(1)
    expect(el('agent-create-profile').value).toBe('readonly-analyst')
  })

  it('offers only the default profile while the list is unknown', () => {
    stubAPI()
    render(<CreateAgentForm users={users} />)
    const field = el('agent-create-profile')
    expect(field.tagName).toBe('SELECT')
    expect(within(field).getAllByRole('option').map(o => o.value))
      .toEqual(['readonly-analyst'])
  })

  it('ignores malformed profile entries', () => {
    stubAPI()
    render(<CreateAgentForm users={users}
      profiles={[null, { name: '' }, { classes: [] }, profiles[1]]} />)
    expect(within(el('agent-create-profile')).getAllByRole('option').map(o => o.value))
      .toEqual(['coding-agent'])
  })

  it('shows the server refusal of an unknown profile', async () => {
    stubAPI({
      'POST /api/v1/agents': () => response(422, {
        error: 'profile "x" is not a known profile; use one of: readonly-analyst' }),
    })
    render(<CreateAgentForm users={users} profiles={profiles} />)
    type('agent-create-name', 'ci-bot')
    type('agent-create-sponsor', '5')
    fireEvent.click(el('agent-create-submit'))
    expect((await screen.findByTestId('agent-create-error')).textContent)
      .toContain('not a known profile')
  })
})
