import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { DatabaseForm } from './DatabaseForm'

afterEach(() => vi.unstubAllGlobals())

describe('Hosted Postgres registration', () => {
  it.each([
    ['neon', 'ep-example.us-east-1.aws.neon.tech', 'neondb', 'project_owner'],
    ['supabase', 'aws-0-us-east-1.pooler.supabase.com', 'postgres', 'postgres.projectref'],
  ])('preserves %s connection fields without inferring permissions', async (
    name, host, database, username,
  ) => {
    const fetch = vi.fn().mockResolvedValue({ ok: true, json: async () => ({}) })
    vi.stubGlobal('fetch', fetch)
    render(<DatabaseForm onClose={vi.fn()} onError={vi.fn()} />)
    for (const [field, value] of [
      ['name', name], ['host', host], ['database', database], ['username', username],
      ['password', 'fixture-password'],
    ]) fireEvent.change(screen.getByTestId(`db-${field}`), { target: { value } })
    fireEvent.click(screen.getByTestId('db-save-button'))
    await waitFor(() => expect(fetch).toHaveBeenCalledOnce())
    const [url, request] = fetch.mock.calls[0]
    expect(url).toBe('/api/v1/databases/managed')
    expect(request.method).toBe('POST')
    expect(JSON.parse(request.body)).toEqual({
      name, host, database_name: database, username, password: 'fixture-password',
      port: 5432, sslmode: 'require', max_connections: 2,
      trust_level: 'observation', execution_mode: 'approval',
    })
  })
})
