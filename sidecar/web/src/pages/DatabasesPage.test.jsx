import { render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { DatabasesPage } from './DatabasesPage'

vi.mock('./databases/DatabaseTable', () => ({
  DatabaseTable: ({ databases }) => (
    <div data-testid="databases-table">{databases.length} rows</div>
  ),
}))

vi.mock('./databases/DatabaseForm', () => ({
  DatabaseForm: () => <div data-testid="database-form" />,
}))

vi.mock('./databases/CSVImport', () => ({
  CSVImport: () => <div data-testid="csv-import" />,
}))

vi.mock('./databases/DeleteConfirm', () => ({
  DeleteConfirm: () => <div data-testid="delete-confirm" />,
}))

describe('DatabasesPage', () => {
  beforeEach(() => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      json: async () => ({ databases: [{ id: 1, name: 'prod' }] }),
    }))
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('describes fleet registration and where cases flow next', async () => {
    render(<DatabasesPage />)

    await waitFor(() => {
      expect(screen.getByTestId('databases-table')).toHaveTextContent('1 rows')
    })
    expect(screen.getByTestId('fleet-page-description'))
      .toHaveTextContent('Register the Postgres fleet')
    expect(screen.getByTestId('fleet-page-description'))
      .toHaveTextContent('cases')
    expect(screen.getByTestId('fleet-page-description'))
      .toHaveTextContent('actions')
  })
})
