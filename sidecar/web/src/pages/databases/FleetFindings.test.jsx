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

  it('flags databases whose counts may be low', async () => {
    respond({ ...recurring, truncated: ['tenant_big', 'tenant_huge'] })
    render(<FleetFindings />)
    await waitFor(() => expect(screen.getByTestId('fleet-findings-truncated'))
      .toHaveTextContent('counts may be low for: tenant_big, tenant_huge'))
  })

  it('says nothing about counts when no database was truncated', async () => {
    respond({ ...recurring, truncated: [] })
    render(<FleetFindings />)
    await screen.findByTestId('fleet-finding-toggle-0')
    expect(screen.queryByTestId('fleet-findings-truncated')).not.toBeInTheDocument()
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
  // fleet-06: each occurrence opens that database's finding case.
  it('links each occurrence to its finding in its database', async () => {
    respond(recurring)
    const select = vi.fn()
    render(<FleetFindings onSelectDatabase={select} />)
    fireEvent.click(await screen.findByTestId('fleet-finding-toggle-0'))
    const link = screen.getByRole('link', { name: /tenant_b.*issue 12/ })
    const [path, query] = link.getAttribute('href').split('?')
    expect(path).toBe('#/cases')
    expect(new URLSearchParams(query).get('finding')).toBe('12')
    expect(new URLSearchParams(query).get('database')).toBe('tenant_b')
    fireEvent.click(link)
    expect(select).toHaveBeenCalledWith('tenant_b')
  })

  it('renders an occurrence without an id as plain text', async () => {
    respond({ ...recurring, findings: [{ ...recurring.findings[0],
      occurrences: [{ database: 'tenant_a', severity: 'warning' }] }] })
    render(<FleetFindings />)
    fireEvent.click(await screen.findByTestId('fleet-finding-toggle-0'))
    expect(screen.getByText('tenant_a')).toBeInTheDocument()
    expect(screen.queryByRole('link')).toBeNull()
  })
})
