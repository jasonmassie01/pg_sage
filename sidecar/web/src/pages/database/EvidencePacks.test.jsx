import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { EvidencePacks } from './EvidencePacks'

// evid-02c: the Evidence packs card. Roles decide the buttons (the API
// enforces the same); "What would leave" renders the model tier's sections
// and charts. No concurrency tests: one person drives one card.

const sha = 'ab'.repeat(32)
const packsURL = '/api/v1/databases/prod/workload-packs'
const pack = {
  content_sha256: sha, format: 'pg_sage-workload/1', tier: 'local', bytes: 2801,
  built_at: '2026-11-15T10:00:00Z', built_by: 'op@example.com',
  window: { from: '2026-10-18T00:00:00Z', to: '2026-11-15T00:00:00Z' },
}
const modelTier = {
  tier: 'model', content_sha256: 'cd'.repeat(32), bytes: 4096, tokens_estimate: 1024,
  hash_identifiers: false,
  sections: {
    'stats.json': { columns: [{ id: 'stats/column/public.orders.token', null_frac: 0.25 }],
      value_samples: false },
    'texts.json': { texts: [{ queryid: '1', text: "SELECT * FROM orders WHERE token = '?'" }] },
  },
  charts: { 'charts/0123456789ab.png': 'data:image/png;base64,iVBORw0KGgo=' },
}

function respond(routes) {
  const calls = []
  globalThis.fetch = vi.fn(async (url, init = {}) => {
    calls.push({ url, method: init.method || 'GET', body: init.body })
    const hit = routes[`${init.method || 'GET'} ${url}`]
    if (!hit) throw new Error(`unexpected request: ${init.method || 'GET'} ${url}`)
    const [status, body] = typeof hit === 'function' ? hit() : hit
    return { ok: status < 400, status, statusText: '', json: async () => body,
      text: async () => JSON.stringify(body) }
  })
  return calls
}

describe('EvidencePacks', () => {
  afterEach(() => { vi.restoreAllMocks() })

  it('lists packs to a viewer without build or export controls', async () => {
    respond({ [`GET ${packsURL}`]: [200, { database: 'prod', packs: [pack] }] })
    render(<EvidencePacks database="prod" user={{ role: 'viewer' }} />)
    await waitFor(() => expect(screen.getByTestId(`pack-${sha.slice(0, 12)}`))
      .toHaveTextContent('2026-10-18 to 2026-11-15'))
    expect(screen.queryByTestId('pack-build')).not.toBeInTheDocument()
    expect(screen.queryByTestId(`pack-export-${sha.slice(0, 12)}`)).not.toBeInTheDocument()
  })

  it('shows an empty list and a list error', async () => {
    respond({ [`GET ${packsURL}`]: [200, { packs: [] }] })
    const { unmount } = render(<EvidencePacks database="prod" />)
    expect(await screen.findByTestId('packs-empty')).toHaveTextContent('No evidence pack')
    unmount()
    respond({ [`GET ${packsURL}`]: [503, { error: 'the database is not connected' }] })
    render(<EvidencePacks database="prod" />)
    expect(await screen.findByTestId('packs-error'))
      .toHaveTextContent('the database is not connected (503)')
  })

  it('lets an operator build a pack and lists it after', async () => {
    let built = false
    const calls = respond({
      [`GET ${packsURL}`]: () => [200, { packs: built ? [pack] : [] }],
      [`POST ${packsURL}`]: () => { built = true; return [201, pack] },
    })
    render(<EvidencePacks database="prod" user={{ role: 'operator' }} />)
    fireEvent.click(await screen.findByTestId('pack-build'))
    expect(await screen.findByTestId(`pack-${sha.slice(0, 12)}`)).toBeInTheDocument()
    expect(calls.filter(c => c.method === 'POST')).toEqual([
      { url: packsURL, method: 'POST', body: '{}' }])
    expect(screen.queryByTestId(`pack-export-${sha.slice(0, 12)}`)).not.toBeInTheDocument()
  })

  it('shows a failed build with the server message', async () => {
    respond({ [`GET ${packsURL}`]: [200, { packs: [] }],
      [`POST ${packsURL}`]: [422, { error: 'workpack: pack too large' }] })
    render(<EvidencePacks database="prod" user={{ role: 'admin' }} />)
    fireEvent.click(await screen.findByTestId('pack-build'))
    expect(await screen.findByTestId('pack-build-error'))
      .toHaveTextContent('workpack: pack too large (422)')
  })

  it('renders what would leave: sections, charts and size', async () => {
    respond({ [`GET ${packsURL}`]: [200, { packs: [pack] }],
      [`GET ${packsURL}/${sha}?tier=model`]: [200, modelTier] })
    render(<EvidencePacks database="prod" user={{ role: 'viewer' }} />)
    fireEvent.click(await screen.findByTestId(`pack-preview-${sha.slice(0, 12)}`))
    expect(await screen.findByTestId('model-tier-size'))
      .toHaveTextContent('4.0 KB, about 1024 tokens, 2 sections, 1 charts')
    expect(screen.getByTestId('model-section-texts.json'))
      .toHaveTextContent("token = '?'")
    const img = screen.getByRole('img', { name: /family 0123456789ab: hour-of-week/ })
    expect(img).toHaveAttribute('src', 'data:image/png;base64,iVBORw0KGgo=')
    expect(screen.queryByTestId('model-tier-pseudonyms')).not.toBeInTheDocument()
  })

  it('shows a refused model tier with its reason', async () => {
    respond({ [`GET ${packsURL}`]: [200, { packs: [pack] }],
      [`GET ${packsURL}/${sha}?tier=model`]: [422, {
        error: 'workpack: the model tier would leak local-tier data: schema.json field ' +
          'tables[0].constraints[0] holds a string literal of the local tier' }] })
    render(<EvidencePacks database="prod" />)
    fireEvent.click(await screen.findByTestId(`pack-preview-${sha.slice(0, 12)}`))
    expect(await screen.findByTestId('model-tier-error'))
      .toHaveTextContent('schema.json field tables[0].constraints[0]')
  })

  it('gives an admin the signed export link of the pack', async () => {
    respond({ [`GET ${packsURL}`]: [200, { packs: [pack] }] })
    render(<EvidencePacks database="prod" user={{ role: 'admin' }} />)
    const link = await screen.findByTestId(`pack-export-${sha.slice(0, 12)}`)
    expect(link).toHaveAttribute('href', `${packsURL}/${sha}/export`)
    expect(link).toHaveTextContent('Download signed export')
    expect(link).toHaveAttribute('download', `pg_sage-workload-prod-${sha.slice(0, 12)}.tar.gz`)
  })

  it('keeps the pseudonym map apart from the tier when names are hashed', async () => {
    respond({ [`GET ${packsURL}`]: [200, { packs: [pack] }],
      [`GET ${packsURL}/${sha}?tier=model`]: [200, { ...modelTier, hash_identifiers: true,
        pseudonyms: { t_0123456789: 'orders' } }] })
    render(<EvidencePacks database="prod" />)
    fireEvent.click(await screen.findByTestId(`pack-preview-${sha.slice(0, 12)}`))
    expect(await screen.findByTestId('model-tier-pseudonyms'))
      .toHaveTextContent('t_0123456789 = orders')
    expect(screen.getByTestId('model-tier-size')).toHaveTextContent('Names are pseudonyms')
  })
})
