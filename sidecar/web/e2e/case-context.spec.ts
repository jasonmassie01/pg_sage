import { test, expect } from '@playwright/test'
import { mockAllAPIs, mockDatabases } from './fixtures'

test('case link selects its database before Ask mounts and changing scope clears case context', async ({ page }) => {
  await mockAllAPIs(page)
  await page.addInitScript(() => localStorage.setItem('pg_sage_db', 'primary'))
  await page.route('**/api/v1/databases', route => route.fulfill({ json: {
    ...mockDatabases,
    databases: mockDatabases.databases.map(db => ({ ...db, status: { ...db.status, trust_level: 'observation' } })),
  } }))
  await page.route('**/api/v1/cases?database=replica', route => route.fulfill({ json: { cases: [{
    id: 'finding:replica:spill:13', database_name: 'replica', source_type: 'finding',
    source_ids: ['13'], title: 'A query used temporary disk space', severity: 'warning', state: 'open',
  }] } }))
  await page.route('**/api/v1/databases/*/ask/**', route => route.fulfill({ json: { conversations: [] } }))
  const asks: string[] = []
  const sessions: string[] = []
  page.on('request', req => {
    if (req.url().includes('/ask')) sessions.push(req.url())
    if (req.method() === 'POST' && req.url().endsWith('/ask')) asks.push(req.url())
  })
  await page.goto('/#/ask?case=finding%3Areplica%3Aspill%3A13&database=replica')
  await expect(page.getByRole('combobox', { name: 'Database scope' })).toHaveValue('replica')
  await expect(page.getByTestId('monitoring-context')).toContainText('Database changes are disabled')
  await expect(page.getByTestId('ask-input')).toHaveValue(/finding 13/)
  expect(asks).toEqual([])
  expect(sessions.some(url => url.includes('/databases/primary/ask'))).toBe(false)
  await page.getByRole('combobox', { name: 'Database scope' }).selectOption('primary')
  await expect(page).toHaveURL(/#\/ask$/)
  await expect(page.getByTestId('ask-case-context')).toHaveCount(0)
  await expect(page.getByTestId('ask-input')).toHaveValue('')
})
