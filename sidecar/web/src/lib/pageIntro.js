// pageIntro.js: the pages on which a viewer has already seen the meme and
// "About this page" guide (ux-05c, decision D5). The guide opens on the first
// visit to each page and collapses to a one-line toggle afterwards. The list
// lives in this browser only, through lib/storage.js.
import { prefsAvailable, readPref, writePref } from './storage'

export const PAGE_INTRO_KEY = 'pg_sage_page_intro_seen'

// Pages are route ids, so the list is bounded by the registry; the cap only
// guards against a store some other code filled.
const MAX_PAGES = 100

// seenPages returns the recorded page ids. A value that is not a JSON list
// is ignored, and so is any entry that is not a string.
function seenPages() {
  const raw = readPref(PAGE_INTRO_KEY, '[]')
  let pages
  try {
    pages = JSON.parse(raw)
  } catch (err) {
    if (err instanceof SyntaxError) return []
    throw err
  }
  return Array.isArray(pages) ? pages.filter(page => typeof page === 'string') : []
}

// isFirstVisit reports whether the guide should open on page. With storage
// blocked a visit cannot be remembered, so the guide stays collapsed rather
// than opening on every page.
export function isFirstVisit(page) {
  return prefsAvailable() && !seenPages().includes(page)
}

// markVisited records page and reports whether the list now holds it. A
// full store cannot record it; the guide then opens again next time.
export function markVisited(page) {
  const pages = seenPages()
  if (pages.includes(page)) return true
  const next = [...pages, page].slice(-MAX_PAGES)
  return writePref(PAGE_INTRO_KEY, JSON.stringify(next))
}
