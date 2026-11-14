// ask-excerpt.spec.ts — mcp-05b CHECK-27: a stored Ask Sage answer shows
// "what Sage read" under a citation, as plain text, and an answer stored
// before excerpts shows no panel. Every /api/* call is stubbed.

import { test, expect, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'

const excerpt = 'finding 42 [warning, open] Missing index on public.orders\n' +
  'detail {"seq_scans":4200} <script>window.pwned = 1</script>'

function storedAnswer(citations: object[]) {
  return {
    id: 41, conversation_id: 'c-1', database: 'primary', question: 'Why is orders slow?',
    status: 'answered', text: 'unused', not_verified: [], dropped: [], actions: [],
    statements: [{ text: 'Sequential scans on public.orders dominate.',
      citations: ['finding:42'], aliases: ['E1'] }],
    citations,
  }
}

async function openStored(page: Page, citations: object[]) {
  await mockAllAPIs(page)
  await page.addInitScript(() => localStorage.setItem('pg_sage_db', 'primary'))
  await page.route('**/api/v1/databases/primary/ask/**', route => {
    const url = route.request().url()
    if (url.includes('/conversations/c-1')) {
      return route.fulfill({ json: { id: 'c-1', answers: [storedAnswer(citations)] } })
    }
    if (url.includes('/conversations')) {
      return route.fulfill({ json: { conversations: [{ id: 'c-1', title: 'Orders' }] } })
    }
    return route.fulfill({ json: {} })
  })
  await page.goto('/#/ask?database=primary')
  await page.getByTestId('ask-conversation-c-1').click()
  await expect(page.getByTestId('ask-citation')).toHaveCount(1)
}

test('a citation shows what Sage read, as text', async ({ page }) => {
  await openStored(page, [{ id: 'finding:42', alias: 'E1', kind: 'finding', ref: '42',
    label: 'finding:42 ok', digest: 'a'.repeat(64), excerpt }])
  const pre = page.getByTestId('ask-citation-excerpt')
  await expect(pre).toBeHidden()
  await page.getByText('What Sage read').click()
  await expect(pre).toBeVisible()
  expect(await pre.textContent()).toBe(excerpt)
  expect(await page.locator('[data-testid="ask-citation"] script').count()).toBe(0)
  expect(await page.evaluate(() => (window as unknown as { pwned?: number }).pwned))
    .toBeUndefined()
})

test('an answer stored before excerpts shows no panel', async ({ page }) => {
  await openStored(page, [{ id: 'finding:42', alias: 'E1', kind: 'finding', ref: '42',
    label: 'finding:42 ok', digest: 'a'.repeat(64) }])
  await expect(page.getByText('What Sage read')).toHaveCount(0)
  await expect(page.getByTestId('ask-citation-excerpt')).toHaveCount(0)
})
