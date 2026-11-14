import { useEffect, useId, useRef } from 'react'

// Dialog is the dashboard's one modal (dashboard-10). It is a native <dialog>
// opened with showModal(), so the browser contains focus and makes the page
// behind it inert. It is named by its title, takes focus on open, returns
// focus to the element that opened it, and cannot be closed while busy.

const FOCUSABLE = 'button:not([disabled]), input:not([disabled]), select:not([disabled]), '
  + 'textarea:not([disabled]), a[href], [tabindex]:not([tabindex="-1"])'

// Chromium lets Tab leave a modal <dialog> for the browser UI after its last
// control, so Tab and Shift+Tab wrap inside the dialog (dashboard-10c).
function wrapTab(el, event) {
  const items = [...el.querySelectorAll(FOCUSABLE)]
  if (items.length === 0) {
    event.preventDefault()
    return
  }
  const first = items[0]
  const last = items[items.length - 1]
  const active = document.activeElement
  const outside = !el.contains(active) || active === el
  if (event.shiftKey && (active === first || outside)) {
    event.preventDefault()
    last.focus()
  } else if (!event.shiftKey && (active === last || outside)) {
    event.preventDefault()
    first.focus()
  }
}

function focusFirst(el) {
  const target = el.querySelector('[data-autofocus]:not([disabled])')
    || el.querySelector(FOCUSABLE) || el
  target.focus()
}

function useModal(ref, open) {
  const returnTo = useRef(null)
  useEffect(() => {
    const el = ref.current
    if (!open || !el) return undefined
    returnTo.current = document.activeElement
    if (!el.open) el.showModal()
    focusFirst(el)
    return () => {
      if (el.open) el.close()
      const back = returnTo.current
      returnTo.current = null
      if (back?.isConnected && typeof back.focus === 'function') back.focus()
    }
  }, [ref, open])
}

// useBusyFocus keeps keyboard focus in the dialog across a busy spell
// (dashboard-14). A busy dialog disables the control that was pressed, and
// the browser then drops focus to the page; when busy ends (an error shown
// in the dialog, say), focus goes back to that control, or to the first one.
function useBusyFocus(ref, open, busy) {
  const lastFocused = useRef(null)
  useEffect(() => {
    const el = ref.current
    if (!open || busy || !el) return
    const active = document.activeElement
    if (el.contains(active) && active !== el) return
    const back = lastFocused.current
    if (back?.isConnected && el.contains(back) && !back.disabled) back.focus()
    else focusFirst(el)
  }, [ref, open, busy])
  return event => {
    if (event.target !== event.currentTarget) lastFocused.current = event.target
  }
}

export function Dialog({
  open, title, onClose, busy = false, testId, children, width = 480,
}) {
  const ref = useRef(null)
  const titleId = useId()
  useModal(ref, open)
  const onFocus = useBusyFocus(ref, open, busy)

  const requestClose = event => {
    event.preventDefault()
    if (!busy) onClose()
  }
  const onKeyDown = event => {
    if (event.key === 'Escape') requestClose(event)
    else if (event.key === 'Tab' && ref.current) wrapTab(ref.current, event)
  }

  return (
    <dialog ref={ref} aria-modal="true" aria-labelledby={titleId}
      data-testid={testId} onCancel={requestClose} onKeyDown={onKeyDown} onFocus={onFocus}
      tabIndex={-1} className="rounded-lg p-0 backdrop:bg-black/60"
      style={{
        width: `min(${width}px, calc(100vw - 32px))`,
        background: 'var(--bg-card)',
        color: 'var(--text-primary)',
        border: '1px solid var(--border)',
      }}>
      {open && (
        <div className="p-5 space-y-4">
          <h2 id={titleId} className="text-base font-semibold"
            style={{ color: 'var(--text-primary)' }}>
            {title}
          </h2>
          {children}
        </div>
      )}
    </dialog>
  )
}
