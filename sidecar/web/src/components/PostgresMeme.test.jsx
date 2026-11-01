import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { PostgresMeme } from './PostgresMeme'

describe('PostgresMeme', () => {
  it('offers captioned original DBA humor with an accessible illustration', () => {
    render(<PostgresMeme route="#/ask" />)
    expect(screen.getByRole('figure', { name: 'PostgreSQL meme for Ask Sage' })).toBeInTheDocument()
    expect(screen.getByRole('img', { name: /elephant DBA/i })).toBeInTheDocument()
    expect(screen.getByText('Just one quick question.')).toBeVisible()
    expect(screen.getByText('Postgres: define “quick.”')).toBeVisible()
  })

  it('keeps the specific advanced page joke rather than the general metrics joke', () => {
    render(<PostgresMeme route="#/advanced/runbooks?database=orders" />)
    expect(screen.getByRole('figure', { name: 'PostgreSQL meme for Runbooks' })).toBeInTheDocument()
    expect(screen.getByText('This is why the DBA writes the runbook.')).toBeVisible()
    expect(screen.queryByText('The cache hit ratio is 99.9%.')).not.toBeInTheDocument()
  })

  it('does not expose auth query parameters in the sign-in caption', () => {
    const { container } = render(<PostgresMeme route="#/link-sso?grant=private-one-time-code" compact />)
    expect(screen.getByRole('figure', { name: 'PostgreSQL meme for Identity' })).toHaveClass('postgres-meme-compact')
    expect(container).not.toHaveTextContent('private-one-time-code')
  })

  it('still supplies a meme when the router displays a missing page', () => {
    render(<PostgresMeme route="#/not-a-real-page" />)
    expect(screen.getByRole('figure', { name: 'PostgreSQL meme for Postgres operations' })).toBeInTheDocument()
    expect(screen.getByText('There is always another nested loop.')).toBeVisible()
  })
})
