import { createMemeDeck } from './postgresMemes'

export const MEME_ROTATION_MS = 60000

// One deck per loaded tab survives page navigation. Draw only on visits, button
// clicks or timer ticks, never React renders (including StrictMode replays).
export function createMemeRotation({ random, paused = false } = {}) {
  const deck = createMemeDeck(random)
  let snapshot = { meme: deck.next(), paused }
  let lastPage
  const listeners = new Set()
  function publish(change) {
    snapshot = { ...snapshot, ...change }
    listeners.forEach(listener => listener())
  }
  function next() {
    publish({ meme: deck.next() })
  }
  return {
    getSnapshot: () => snapshot,
    subscribe(listener) {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
    visit(page) {
      if (lastPage !== undefined && page !== lastPage) next()
      lastPage = page
    },
    next,
    togglePaused: () => publish({ paused: !snapshot.paused }),
  }
}

export const memeRotation = createMemeRotation({
  paused: typeof window !== 'undefined'
    && Boolean(window.matchMedia?.('(prefers-reduced-motion: reduce)').matches),
})
