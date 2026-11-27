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
  // The page asks through an asynchronous run (mcp-07b): it starts one, then
  // polls it until it is done with the answer.
  await page.route('**/api/v1/databases/primary/ask/runs', async route => {
    expect(route.request().method()).toBe('POST')
    expect(route.request().postDataJSON()).toEqual({ question: 'Which indexes need attention?' })
    await route.fulfill({ status: 202, json: { run_id: 'run-1', conversation_id: 'observatory-1' } })
  })
  await page.route('**/api/v1/databases/primary/ask/runs/run-1', route => route.fulfill({ json: {
    id: 'run-1', state: 'done', progress: [], conversation_id: 'observatory-1', answer: {
      id: 1, conversation_id: 'observatory-1', question: 'Which indexes need attention?', status: 'answered',
      statements: [{ text: 'The duplicate index needs review.', citations: ['finding-1'] }],
      citations: [{ id: 'finding-1', kind: 'finding', label: 'Duplicate index evidence' }],
    },
  } }))
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

// ux-05c (decision D5): the meme and its "What's here" guide open on the first
// visit to each page, then collapse to a one-line "About this page" toggle;
// below 768 px they are hidden. Aliases share their page's visit.
// ux-11: one path per page; the old paths are aliases.
const routes = ['/', '/issues', '/changes', '/slos', '/autonomy', '/facts', '/ask',
  '/manage-databases', '/settings', '/advanced/runbooks', '/database',
  '/notifications', '/profile', '/users', '/agents', '/unknown-route']
const aliases = ['/advanced/findings', '/advanced/actions', '/cases', '/actions', '/trust',
  '/advanced/autonomy', '/advanced', '/alerts', '/mcp-tokens']

async function firstVisitShowsGuide(page: Page, route: string, wide: boolean) {
  await page.goto('/#' + route)
  await expect(page.getByRole('button', { name: 'About this page' }), route)
    .toHaveAttribute('aria-expanded', 'true')
  const meme = page.getByTestId('postgres-meme')
  await expect(meme).toHaveCount(1)
  await expect(meme).toBeInViewport({ ratio: 1 })
  const introduction = page.getByTestId('tab-introduction')
  await expect(introduction).toBeVisible()
  await expect(introduction.getByRole('heading', { level: 2 })).toContainText('About ')
  if (wide) {
    const [imageBox, introBox] = await Promise.all([
      meme.getByRole('img').boundingBox(), introduction.boundingBox()])
    expect(introBox!.x).toBeGreaterThan(imageBox!.x + imageBox!.width)
    expect(introBox!.y).toBeLessThan(imageBox!.y + imageBox!.height)
  }
  const image = meme.getByRole('img')
  await expect(image).toHaveCount(1)
  // dashboard-09: memes are Vite-hashed bundle assets served from /assets/.
  await expect(image).toHaveAttribute('src', /^\/assets\/[^/]+-[\w-]+\.jpg$/)
  await expect.poll(() => image.evaluate((element: HTMLImageElement) =>
    element.complete && element.naturalWidth > 0 && element.naturalHeight > 0
      && new URL(element.currentSrc).origin === window.location.origin
  ), { message: `local meme image decodes on ${route}` }).toBe(true)
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), route)
    .toBe(true)
}

// gapAboveContent is the distance from the bottom of the database context bar
// to the top of the page's own content: on a phone, nothing sits between them.
function gapAboveContent(page: Page) {
  return page.evaluate(() => {
    const area = document.querySelector('.sage-content')!
    const context = area.querySelector('[data-testid="monitoring-context"]')!
    return area.lastElementChild!.getBoundingClientRect().top - context.getBoundingClientRect().bottom
  })
}

for (const viewport of [{ width: 1280, height: 1000 }, { width: 768, height: 1000 }]) {
  test(`every signed-in page opens its guide once, then collapses, at ${viewport.width}px`,
    async ({ page }) => {
      test.setTimeout(120000)
      await page.setViewportSize(viewport)
      await singletonApp(page)
      const errors: string[] = []
      page.on('pageerror', error => errors.push(error.message))
      for (const route of routes) await firstVisitShowsGuide(page, route, viewport.width === 1280)
      const toggle = page.getByRole('button', { name: 'About this page' })
      for (const route of [...routes, ...aliases]) {
        await page.goto('/#' + route)
        await expect(toggle, route).toHaveAttribute('aria-expanded', 'false')
        await expect(toggle).toBeInViewport()
        await expect(page.getByTestId('postgres-meme')).toHaveCount(0)
        await expect(page.getByTestId('tab-introduction')).toHaveCount(0)
      }
      // Opened by the viewer, then collapsed again on the next page.
      await toggle.click()
      await expect(toggle).toHaveAttribute('aria-expanded', 'true')
      await expect(page.getByTestId('tab-introduction')).toBeVisible()
      await page.goto('/#/cases')
      await expect(toggle).toHaveAttribute('aria-expanded', 'false')
      await expect(page.getByTestId('postgres-meme')).toHaveCount(0)
      expect(errors).toEqual([])
    })
}

for (const width of [375, 767]) {
  test(`phones show no meme or guide at ${width}px and keep the inbox near the top`,
    async ({ page }) => {
      test.setTimeout(90000)
      await page.setViewportSize({ width, height: 844 })
      await singletonApp(page)
      const errors: string[] = []
      page.on('pageerror', error => errors.push(error.message))
      for (const route of routes) {
        await page.goto('/#' + route)
        await expect(page.locator('header h1')).not.toBeEmpty()
        await expect(page.getByTestId('postgres-meme'), route).toHaveCount(0)
        await expect(page.getByRole('button', { name: 'About this page' })).toHaveCount(0)
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), route)
          .toBe(true)
      }
      await page.goto('/#/')
      await expect(page.getByTestId('inbox-page')).toBeVisible()
      // Only the context bar's own 16 px margin; the meme block took 200+ px here.
      expect(await gapAboveContent(page)).toBeLessThanOrEqual(16)
      // A visit on a phone is not a visit to the guide: widening the window to
      // 768 px opens it on this page.
      await page.setViewportSize({ width: 768, height: 844 })
      await expect(page.getByRole('button', { name: 'About this page' }))
        .toHaveAttribute('aria-expanded', 'true')
      expect(errors).toEqual([])
    })
}

// The guide is hidden below 768 px (ux-05c), so the deck runs where it shows.
for (const viewport of [{ width: 1440, height: 1000 }, { width: 768, height: 1000 }]) {
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
  // ux-05c: phones do not show the meme block (decision D5).
  await expect(page.getByTestId('postgres-meme')).toHaveCount(0)
})
