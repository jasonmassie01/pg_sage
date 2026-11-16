// Workload evidence pack payloads (evid-02c), in the shape of the Go
// handlers: GET .../workload-packs ({database, packs}), POST (the stored
// row) and GET .../{sha}?tier=model (workpack.TierView plus pack and
// hash_identifiers, internal/api/workload_pack_tiers.go).

import type { Page } from '@playwright/test'

export const packSHA = 'ab'.repeat(32)

export const storedPack = {
  id: 1, content_sha256: packSHA, format: 'pg_sage-workload/1', tier: 'local',
  bytes: 2801, built_at: '2026-11-15T10:00:00Z', built_by: 'test@test.com',
  reused: false, window: { from: '2026-10-18T00:00:00Z', to: '2026-11-15T00:00:00Z' },
}

// A 1 x 1 transparent PNG stands in for a 336 x 64 sparkline.
const png = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4' +
  'nGNgYGD4DwABBAEAwS2OUAAAAABJRU5ErkJggg=='

export const modelTier = {
  tier: 'model', content_sha256: 'cd'.repeat(32), bytes: 19956, tokens_estimate: 4989,
  manifest: { format: 'pg_sage-workload/1', tier: 'model', database: 'primary', files: [] },
  pack: storedPack, hash_identifiers: false,
  sections: {
    'stats.json': { columns: [{ id: 'stats/column/public.orders.token', table: 'public.orders',
      column: 'token', null_frac: 0.25, n_distinct: -1, correlation: null, avg_width: 24 }],
    value_samples: false, tables: [], truncated: false, columns_listed: 1 },
    'texts.json': { texts: [{ queryid: '1', family: 'workload/family/0123456789ab',
      text: "SELECT * FROM orders WHERE token = '?' AND card = ?", truncated: false }] },
    'tier.json': { format: 'pg_sage-workload/1', tier: 'model',
      source_content_sha256: packSHA, hash_identifiers: false },
  },
  charts: { 'charts/0123456789ab.png': png, 'charts/0123456789ab-growth.png': png },
}

// routePacks serves the pack routes of database; built says whether the
// list holds the pack yet (a POST builds it). It returns the requests seen.
export async function routePacks(page: Page, database: string, built = false) {
  const seen: { method: string, url: string }[] = []
  const base = `/api/v1/databases/${database}/workload-packs`
  await page.route(`**${base}**`, route => {
    const req = route.request()
    const url = new URL(req.url())
    seen.push({ method: req.method(), url: url.pathname + url.search })
    if (req.method() === 'POST') {
      built = true
      return route.fulfill({ status: 201, json: storedPack })
    }
    if (url.pathname === base) {
      return route.fulfill({ json: { database, packs: built ? [storedPack] : [] } })
    }
    if (url.pathname === `${base}/${packSHA}` && url.searchParams.get('tier') === 'model') {
      return route.fulfill({ json: modelTier })
    }
    if (url.pathname === `${base}/${packSHA}/export`) {
      return route.fulfill({ status: 200, body: Buffer.from([0x1f, 0x8b, 0x08, 0x00]),
        headers: { 'Content-Type': 'application/gzip',
          'Content-Disposition': `attachment; filename="pg_sage-workload-${database}-` +
            `${packSHA.slice(0, 12)}.tar.gz"` } })
    }
    return route.fulfill({ status: 404, json: { error: 'workload pack not found' } })
  })
  return seen
}
