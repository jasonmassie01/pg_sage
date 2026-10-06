import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { FleetFindings } from './FleetFindings'

function respond(body, ok = true) {
  globalThis.fetch = vi.fn(async url => {
    if (url !== '/api/v1/fleet/findings') {
      throw new Error(`unexpected request: ${url}`)
    }
    return { ok, status: ok ? 200 : 500, json: async () => body }
  })
}

const recurring = {
  min_databases: 3,
  databases_scanned: 4,
  errors: [],
  findings: [{
    key: 'missing_index|public.orders|btree(status)',
    category: 'missing_index',
    object_identifier: 'public.orders|btree(status)',
    title: 'Missing index on public.orders',
    severity: 'warning',
    databases: 3,
    occurrences: [
      { database: 'tenant_a', id: 11, severity: 'warning' },
      { database: 'tenant_b', id: 12, severity: 'warning' },
      { database: 'tenant_c', id: 13, severity: 'critical' },
    ],
  }],
}

describe('FleetFindings', () => {
  afterEach(() => { vi.restoreAllMocks() })

  it('lists a recurring problem once with its database count', async () => {
    respond(recurring)
    render(<FleetFindings />)
    await waitFor(() => expect(screen.getByTestId('fleet-findings'))
      .toHaveTextContent('Missing index on public.orders'))
    expect(screen.getByTestId('fleet-finding-count-0'))
      .toHaveTextContent('3 databases')
    expect(screen.queryByText('tenant_a')).not.toBeInTheDocument()
  })

  it('drills down to the affected databases', async () => {
    respond(recurring)
    render(<FleetFindings />)
    const toggle = await screen.findByTestId('fleet-finding-toggle-0')
    fireEvent.click(toggle)
    expect(screen.getByText('tenant_a')).toBeInTheDocument()
    expect(screen.getByText('tenant_c')).toBeInTheDocument()
    fireEvent.click(toggle)
    expect(screen.queryByText('tenant_a')).not.toBeInTheDocument()
  })

  it('says when nothing recurs', async () => {
    respond({ ...recurring, findings: [] })
    render(<FleetFindings />)
    await waitFor(() => expect(screen.getByTestId('fleet-findings-empty'))
      .toHaveTextContent('3 or more databases'))
  })

  it('names databases that could not be read', async () => {
    respond({ ...recurring, errors: [{ database: 'tenant_x', error: 'timeout' }] })
    render(<FleetFindings />)
    await waitFor(() => expect(screen.getByTestId('fleet-findings-partial'))
      .toHaveTextContent('tenant_x'))
  })

  it('shows a load failure instead of an empty list', async () => {
    respond({ error: 'boom' }, false)
    render(<FleetFindings />)
    await waitFor(() => expect(screen.getByTestId('fleet-findings-error'))
      .toBeInTheDocument())
    expect(screen.queryByTestId('fleet-findings-empty')).not.toBeInTheDocument()
  })

  it('survives a malformed response', async () => {
    respond({ findings: 'nope' })
    render(<FleetFindings />)
    await waitFor(() => expect(screen.getByTestId('fleet-findings-empty'))
      .toBeInTheDocument())
  })
})
