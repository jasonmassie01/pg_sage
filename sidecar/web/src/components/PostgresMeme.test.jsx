import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { PostgresMeme } from './PostgresMeme'

// Include every App route, including older links into the unified Cases page.
const routes = [
  '#/', '#/advanced', '#/advanced/findings', '#/advanced/actions',
  '#/advanced/runbooks', '#/advanced/autonomy', '#/manage-databases', '#/database',
  '#/cases', '#/findings', '#/actions', '#/slos', '#/trust', '#/facts', '#/ask',
  '#/settings', '#/notifications', '#/alerts', '#/alert-log', '#/users', '#/profile',
  '#/agents', '#/mcp-tokens', '#/link-sso', '#/login', '#/forecasts', '#/query-hints',
  '#/schema-health', '#/incidents',
]

describe('PostgresMeme', () => {
  it('uses the recognizable Drake image with accessible HTML captions', () => {
    render(<PostgresMeme route="#/ask" />)
    expect(screen.getByRole('figure', { name: 'PostgreSQL meme for Ask Sage' })).toBeInTheDocument()
    const image = screen.getByRole('img', { name: /Drake rejects the top option/i })
    expect(image).toHaveAttribute('src', '/memes/drake.jpg')
    expect(screen.getByText(/“IT’S PROBABLY THE CACHE”/)).toBeVisible()
    expect(screen.getByText(/EXPLAIN \(ANALYZE, BUFFERS\)/)).toBeVisible()
    expect(screen.getByText('Drake rejects:')).toHaveClass('sr-only')
    expect(screen.getByText('Drake approves:')).toHaveClass('sr-only')
  })

  it('keeps the specific advanced page joke rather than the general metrics joke', () => {
    render(<PostgresMeme route="#/advanced/runbooks?database=orders" />)
    expect(screen.getByRole('figure', { name: 'PostgreSQL meme for Runbooks' })).toBeInTheDocument()
    expect(screen.getByText(/A RUNBOOK THAT WORKS WHILE DAVE IS ON PTO/)).toBeVisible()
    expect(screen.queryByText(/CPU: 100%/)).not.toBeInTheDocument()
  })

  it('does not expose auth query parameters in the sign-in caption', () => {
    const { container } = render(<PostgresMeme route="#/link-sso?grant=private-one-time-code" compact />)
    expect(screen.getByRole('figure', { name: 'PostgreSQL meme for Identity' })).toHaveClass('postgres-meme-compact')
    expect(container).not.toHaveTextContent('private-one-time-code')
    expect(container.innerHTML).not.toContain('private-one-time-code')
  })

  it('still supplies a familiar meme when the router displays a missing page', () => {
    render(<PostgresMeme route="#/not-a-real-page" />)
    expect(screen.getByRole('figure', { name: 'PostgreSQL meme for Postgres operations' })).toBeInTheDocument()
    expect(screen.getByRole('img', { name: /cartoon dog sits in a burning room/i })).toBeInTheDocument()
  })

  it.each(routes)('has a locally bundled, lazy-loaded template and captions on %s', route => {
    const { container } = render(<PostgresMeme route={route} compact />)
    const image = screen.getByRole('img')
    const src = image.getAttribute('src')
    expect(src).toMatch(/^\/memes\/[a-z-]+\.jpg$/)
    expect(image).toHaveAttribute('loading', 'lazy')
    expect(image).toHaveAttribute('decoding', 'async')
    expect(Number(image.getAttribute('width'))).toBeGreaterThan(0)
    expect(Number(image.getAttribute('height'))).toBeGreaterThan(0)
    const bundled = readFileSync(resolve('public', src.slice(1)))
    expect([...bundled.subarray(0, 3)]).toEqual([0xff, 0xd8, 0xff])
    expect(container.querySelectorAll('.postgres-meme-label').length).toBeGreaterThan(0)
    expect(screen.getByRole('link', { name: 'Template credits' })).toHaveAttribute('href', '/memes/CREDITS.md')
  })
})
