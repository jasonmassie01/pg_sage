// ask.ts: Ask Sage payloads and routes for the dashboard-13 specs.
// Every payload names the Go test (or struct) whose JSON it copies.

import { type Page, type Request } from '@playwright/test'

// citation copies ask.Citation (internal/ask/types.go) as
// internal/ask/answer_alias_test.go stores it: alias E<n>, kind, ref.
export function citation(alias: string, kind: string, ref: string) {
  return {
    id: `${kind}:${ref}`, alias, kind, ref, label: `${kind}:${ref} ok`,
    digest: `${alias.toLowerCase()}aaaaaaaaaaaaaaaa`,
  }
}

// askAnswer copies ask.Answer as TestAskRoutes_ReadOnlyIsAcceptedAndOffersNoWriteTools
// stores it: read_only is set when the question was sent answer-only.
export function askAnswer(question: string, opts: {
  readOnly?: boolean, citations?: ReturnType<typeof citation>[],
} = {}) {
  const citations = opts.citations ?? []
  return {
    id: 101, conversation_id: 'conv-1', database: 'orders', question,
    status: 'answered', text: '',
    statements: [{
      text: 'The table statistics of public.t12 are out of date.',
      citations: citations.map(c => c.id), aliases: citations.map(c => c.alias),
    }],
    not_verified: [], dropped: [], citations, actions: [], stop: 'end_turn', tokens: 812,
    ...(opts.readOnly ? { read_only: true } : {}),
    created_at: '2026-11-12T10:30:00Z',
  }
}

// routeAsk answers POST /databases/{db}/ask with answer(body) and the
// session's GETs (budget, conversations) with empty lists. It returns the
// POSTed requests so a spec can check what was sent.
export async function routeAsk(page: Page,
  answer: (body: Record<string, unknown>) => unknown) {
  const posts: Request[] = []
  await page.route('**/api/v1/databases/*/ask**', route => {
    const req = route.request()
    if (req.method() === 'POST' && new URL(req.url()).pathname.endsWith('/ask')) {
      posts.push(req)
      return route.fulfill({ json: answer(req.postDataJSON()) })
    }
    return route.fulfill({ json: { conversations: [] } })
  })
  return posts
}
