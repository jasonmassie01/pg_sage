import React from 'react'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { DatabaseForm } from './DatabaseForm'

// security-09b: the optional action identity is write-only. The form sends
// action_username / action_password only when the operator fills them, and
// never shows a stored password.

function mockSave() {
  return vi.spyOn(globalThis, 'fetch').mockResolvedValue({
    ok: true,
    json: async () => ({}),
  })
}

async function fillRequired(user) {
  await user.type(screen.getByTestId('db-name'), 'prod')
  await user.type(screen.getByTestId('db-host'), 'db.example.com')
  await user.type(screen.getByTestId('db-database'), 'app')
  await user.type(screen.getByTestId('db-username'), 'sage')
  await user.type(screen.getByTestId('db-password'), 'secret')
}

function sentBody(fetchMock) {
  return JSON.parse(fetchMock.mock.calls[0][1].body)
}

const stored = {
  id: 7,
  name: 'prod',
  host: 'db.example.com',
  database_name: 'app',
  username: 'sage',
  trust_level: 'observation',
  execution_mode: 'approval',
  action_identity: { configured: true, username: 'sage_actor' },
}

describe('DatabaseForm action identity', () => {
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('omits the action fields when they are left empty', async () => {
    const user = userEvent.setup()
    const fetchMock = mockSave()
    render(<DatabaseForm onClose={vi.fn()} onError={vi.fn()} />)
    await fillRequired(user)
    await user.click(screen.getByTestId('db-save-button'))
    await waitFor(() => expect(fetchMock).toHaveBeenCalled())
    const body = sentBody(fetchMock)
    expect(body).not.toHaveProperty('action_username')
    expect(body).not.toHaveProperty('action_password')
  })

  it('sends both fields when the operator fills them', async () => {
    const user = userEvent.setup()
    const fetchMock = mockSave()
    render(<DatabaseForm onClose={vi.fn()} onError={vi.fn()} />)
    await fillRequired(user)
    await user.type(screen.getByTestId('db-action-username'), 'sage_actor')
    await user.type(screen.getByTestId('db-action-password'), 'act-secret')
    await user.click(screen.getByTestId('db-save-button'))
    await waitFor(() => expect(fetchMock).toHaveBeenCalled())
    const body = sentBody(fetchMock)
    expect(body.action_username).toBe('sage_actor')
    expect(body.action_password).toBe('act-secret')
  })

  it('shows the stored role but never a stored password', () => {
    render(<DatabaseForm db={stored} onClose={vi.fn()} onError={vi.fn()} />)
    expect(screen.getByTestId('db-action-username')).toHaveValue('sage_actor')
    const pw = screen.getByTestId('db-action-password')
    expect(pw).toHaveValue('')
    expect(pw).toHaveAttribute('type', 'password')
    expect(pw).toHaveAttribute('placeholder', '(unchanged)')
    expect(screen.getByText(/second role on the same database/i)).toBeInTheDocument()
  })

  it('leaves an unchanged stored identity out of an edit', async () => {
    const user = userEvent.setup()
    const fetchMock = mockSave()
    render(<DatabaseForm db={stored} onClose={vi.fn()} onError={vi.fn()} />)
    await user.click(screen.getByTestId('db-save-button'))
    await waitFor(() => expect(fetchMock).toHaveBeenCalled())
    const body = sentBody(fetchMock)
    expect(body).not.toHaveProperty('action_username')
    expect(body).not.toHaveProperty('action_password')
  })

  it('sends an empty username to remove a stored identity', async () => {
    const user = userEvent.setup()
    const fetchMock = mockSave()
    render(<DatabaseForm db={stored} onClose={vi.fn()} onError={vi.fn()} />)
    await user.clear(screen.getByTestId('db-action-username'))
    await user.click(screen.getByTestId('db-save-button'))
    await waitFor(() => expect(fetchMock).toHaveBeenCalled())
    const body = sentBody(fetchMock)
    expect(body.action_username).toBe('')
    expect(body).not.toHaveProperty('action_password')
  })

  it('sends the role with a new password even when the role is unchanged', async () => {
    const user = userEvent.setup()
    const fetchMock = mockSave()
    render(<DatabaseForm db={stored} onClose={vi.fn()} onError={vi.fn()} />)
    await user.type(screen.getByTestId('db-action-password'), 'rotated')
    await user.click(screen.getByTestId('db-save-button'))
    await waitFor(() => expect(fetchMock).toHaveBeenCalled())
    const body = sentBody(fetchMock)
    expect(body.action_username).toBe('sage_actor')
    expect(body.action_password).toBe('rotated')
  })
})
