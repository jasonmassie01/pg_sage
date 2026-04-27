import React from 'react'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { DatabaseForm } from './DatabaseForm'

describe('DatabaseForm', () => {
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('submits max_connections as a number', async () => {
    const user = userEvent.setup()
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true,
      json: async () => ({}),
    })
    const onClose = vi.fn()

    render(<DatabaseForm onClose={onClose} onError={vi.fn()} />)

    await user.type(screen.getByTestId('db-name'), 'prod')
    await user.type(screen.getByTestId('db-host'), 'db.example.com')
    await user.type(screen.getByTestId('db-database'), 'app')
    await user.type(screen.getByTestId('db-username'), 'sage')
    await user.type(screen.getByTestId('db-password'), 'secret')
    await user.clear(screen.getByTestId('db-max-connections'))
    await user.type(screen.getByTestId('db-max-connections'), '17')
    await user.click(screen.getByTestId('db-save-button'))

    await waitFor(() => expect(fetchMock).toHaveBeenCalled())

    const body = JSON.parse(fetchMock.mock.calls[0][1].body)
    expect(body.max_connections).toBe(17)
    expect(typeof body.max_connections).toBe('number')
  })
})
