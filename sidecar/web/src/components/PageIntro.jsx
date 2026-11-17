import { useEffect, useState, useSyncExternalStore } from 'react'
import { ChevronDown, ChevronRight } from 'lucide-react'
import { PostgresMeme } from './PostgresMeme'
import { findRoute } from '../routes'
import { isFirstVisit, markVisited } from '../lib/pageIntro'

// PageIntro holds the meme and "What's here" guide above every signed-in
// page (ux-05c, decision D5). It opens on the first visit to each page and
// is a one-line "About this page" toggle afterwards; a page the viewer opens
// collapses again on the next page. Below 768 px it is not shown at all.

export const PHONE_QUERY = '(max-width: 767px)'

function subscribePhone(onChange) {
  const query = window.matchMedia?.(PHONE_QUERY)
  if (!query) return () => {}
  query.addEventListener('change', onChange)
  return () => query.removeEventListener('change', onChange)
}

// A browser without matchMedia is treated as wider than a phone.
const isPhone = () => Boolean(window.matchMedia?.(PHONE_QUERY).matches)

// pageKey names the page a route opens: aliases and query strings share it,
// and every unrouted address is one "not-found" page.
function pageKey(route) {
  const match = findRoute(route || '#/')
  return match ? match.id : 'not-found'
}

export function PageIntro({ route, guide, rotation }) {
  const page = pageKey(route)
  const phone = useSyncExternalStore(subscribePhone, isPhone)
  const [state, setState] = useState(() => ({ page, open: isFirstVisit(page) }))
  // A new page starts from its own first-visit rule, not the last page's toggle.
  if (state.page !== page) setState({ page, open: isFirstVisit(page) })
  // A page hidden on a phone was not seen, so the visit is not recorded.
  useEffect(() => { if (!phone) markVisited(page) }, [page, phone])

  if (phone) return null
  const open = state.page === page && state.open
  return (
    <section className="page-intro" data-testid="page-intro">
      <button type="button" className="page-intro-toggle" aria-expanded={open}
        aria-controls={open ? 'page-intro-body' : undefined} data-testid="page-intro-toggle"
        onClick={() => setState({ page, open: !open })}>
        {open ? <ChevronDown size={13} aria-hidden="true" />
          : <ChevronRight size={13} aria-hidden="true" />}
        About this page
      </button>
      {open && (
        <div id="page-intro-body">
          <PostgresMeme route={route} guide={guide} rotation={rotation} />
        </div>
      )}
    </section>
  )
}
