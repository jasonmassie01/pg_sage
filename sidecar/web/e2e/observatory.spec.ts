import { test, expect, type Page } from '@playwright/test'
import { mockAllAPIs, mockDatabases, mockAuthMe } from './fixtures'

async function singletonApp(page: Page) {
  await mockAllAPIs(page)
  await page.route('**/api/v1/auth/mode', route => route.fulfill({ json: { mode: 'databricks_app' } }))
  await page.route('**/api/v1/auth/me', route => route.fulfill({ json: { ...mockAuthMe, auth_mode: 'databricks_app' } }))
  await page.route('**/api/v1/databases', route => route.fulfill({ json: {
    ...mockDatabases, databases: [mockDatabases.databases[0]],
    summary: { ...mockDatabases.summary, total_databases: 1 },
  } }))
  await page.route('**/api/v1/databases/managed', route => route.fulfill({ json: { databases: [] } }))
  await page.route('**/api/v1/config/global', route => route.fulfill({ json: { config: { mode: 'standalone' } } }))
  await page.route('**/api/v1/databases/primary/ask/**', route => route.fulfill({ json: { conversations: [] } }))
  // Reproduce the stored scope that previously caused the hidden-picker dead end.
  await page.addInitScript(() => localStorage.setItem('pg_sage_db', 'all'))
}

test('singleton Ask sends the question to the selected database and renders evidence', async ({ page }) => {
  await singletonApp(page)
  await page.route('**/api/v1/databases/primary/ask', async route => {
    expect(route.request().method()).toBe('POST')
    expect(route.request().postDataJSON()).toEqual({ question: 'Which indexes need attention?' })
    await route.fulfill({ json: {
      id: 1, conversation_id: 'observatory-1', question: 'Which indexes need attention?', status: 'answered',
      statements: [{ text: 'The duplicate index needs review.', citations: ['finding-1'] }],
      citations: [{ id: 'finding-1', kind: 'finding', label: 'Duplicate index evidence' }],
    } })
  })
  await page.goto('/#/ask')
  await expect(page.getByRole('combobox', { name: 'Database scope' })).toHaveValue('primary')
  await expect(page.getByTestId('ask-pick-database')).toHaveCount(0)
  await page.getByTestId('ask-input').fill('Which indexes need attention?')
  await page.getByTestId('ask-submit').click()
  await expect(page.getByTestId('ask-statement')).toContainText('duplicate index')
  await expect(page.getByTestId('ask-citation')).toContainText('Duplicate index evidence')
})

test('Fleet shows the configured runtime and opens its database scope', async ({ page }) => {
  await singletonApp(page)
  await page.goto('/#/manage-databases')
  await expect(page.getByTestId('fleet-inventory-summary')).toContainText('1')
  await expect(page.getByTestId('config-file-databases')).toContainText('primary')
  await expect(page.getByTestId('config-file-databases')).toContainText('connected')
  await expect(page.getByTestId('fleet-inventory-empty')).toHaveCount(0)
  await page.getByRole('link', { name: 'Inspect database' }).click()
  await expect(page).toHaveURL(/#\/database$/)
  await expect(page.getByRole('combobox', { name: 'Database scope' })).toHaveValue('primary')
})

const routes = ['/', '/cases', '/actions', '/slos', '/trust', '/facts', '/ask',
  '/manage-databases', '/settings', '/advanced', '/advanced/findings',
  '/advanced/actions', '/advanced/runbooks', '/advanced/autonomy', '/database',
  '/alerts', '/notifications', '/profile', '/users', '/mcp-tokens', '/agents', '/unknown-route']

for (const viewport of [{ width: 1440, height: 1000 }, { width: 390, height: 844 }]) {
  test(`every signed-in page has its meme and fits ${viewport.width}px`, async ({ page }) => {
    test.setTimeout(90000)
    await page.setViewportSize(viewport)
    await singletonApp(page)
    const errors: string[] = []
    page.on('pageerror', error => errors.push(error.message))
    for (const route of routes) {
      await page.goto('/#' + route)
      const meme = page.getByTestId('postgres-meme')
      await expect(meme).toHaveCount(1)
      await expect(meme).toBeInViewport({ ratio: 1 })
      const image = meme.getByRole('img')
      await expect(image).toHaveCount(1)
      await expect(image).toHaveAttribute('src', /^\/memes\/[^/]+\.jpg$/)
      await expect.poll(() => image.evaluate((element: HTMLImageElement) =>
        element.complete && element.naturalWidth > 0 && element.naturalHeight > 0
          && new URL(element.currentSrc).origin === window.location.origin
      ), { message: `local meme image decodes on ${route} at ${viewport.width}px` }).toBe(true)
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), route).toBe(true)
    }
    expect(errors).toEqual([])
  })
}

for (const viewport of [{ width: 1440, height: 1000 }, { width: 390, height: 844 }]) {
  test(`all 50 shuffled memes fit near the top at ${viewport.width}px`, async ({ page }) => {
    await page.setViewportSize(viewport)
    await singletonApp(page)
    await page.goto('/#/ask')
    const meme = page.getByTestId('postgres-meme')
    const seen = new Set<string>()
    const images = new Set<string>()
    for (let i = 0; i < 50; i++) {
      await expect(meme).toBeInViewport({ ratio: 1 })
      const id = await meme.getAttribute('data-meme-id')
      expect(id).toBeTruthy()
      expect(seen.has(id!)).toBe(false)
      seen.add(id!)
      const image = meme.getByRole('img')
      await expect.poll(() => image.evaluate((el: HTMLImageElement) => el.complete && el.naturalWidth > 0)).toBe(true)
      images.add((await image.getAttribute('src'))!)
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
      await meme.getByRole('button', { name: 'Another meme' }).click()
    }
    expect(seen.size).toBe(50)
    expect(images.size).toBe(6)
  })
}

test('standalone Settings loads instance configuration for the running ID-less database', async ({ page }) => {
  await singletonApp(page)
  const db = { ...mockDatabases.databases[0], id: 0, database_id: 0 }
  await page.route('**/api/v1/databases', route => route.fulfill({ json: {
    ...mockDatabases, databases: [db],
  } }))
  await page.route('**/api/v1/config/global', route => route.fulfill({ json: {
    mode: 'standalone', config: { auth: { mode: 'databricks_app' }, postgres: { max_connections: 5 } }, generation: 12,
  } }))
  await page.goto('/#/settings')
  await expect(page.getByText(/Instance settings for primary \(standalone\)/)).toBeVisible()
  await expect(page.getByText(/Selected database .* is not available/)).toHaveCount(0)
})

test('mobile navigation closes after choosing Ask Sage', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await singletonApp(page)
  await page.goto('/#/manage-databases')
  await page.getByTestId('sidebar-open').click()
  const drawer = page.getByTestId('sidebar-drawer')
  await expect(drawer).toBeVisible()
  await drawer.getByTestId('nav-ask').click()
  await expect(drawer).toHaveCount(0)
  await expect(page.getByTestId('ask-input')).toBeVisible()
  await expect(page.getByTestId('postgres-meme')).toBeVisible()
})
