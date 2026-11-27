import { useEffect, useRef } from 'react'

// Tabs.jsx is the dashboard's one tab bar (ux-05d): a named tablist of
// tabs with a roving tab stop, Arrow Left and Right (wrapping), Home and
// End, and selection that follows focus; TabPanel is the region the
// selected tab controls. Pages keep their own look through className,
// tabClassName and tabStyle. A panel with nothing focusable inside is
// itself the Tab stop after the bar (ux-12), as the WAI-ARIA tabs pattern
// asks, so an empty list is still reached and read.

const KEYS = ['ArrowRight', 'ArrowLeft', 'Home', 'End']

// tabIds turns a bar id and a tab key into the element ids that link the
// tab and its panel; keys like "Safety & trust" become valid ids.
function tabIds(id, key) {
  const safe = String(key).replace(/[^A-Za-z0-9_-]+/g, '-')
  return { tab: `${id}-tab-${safe}`, panel: `${id}-panel-${safe}` }
}

function nextIndex(key, index, count) {
  if (key === 'Home') return 0
  if (key === 'End') return count - 1
  if (key === 'ArrowRight') return (index + 1) % count
  return (index - 1 + count) % count
}

// tabs: [{ key, label, testId }]; label may be any node.
export function TabList({
  id, tabs, active, onSelect, label, className = 'flex flex-wrap gap-2',
  tabClassName = 'px-3 py-1.5 rounded text-sm', tabStyle, ...rest
}) {
  if (!tabs?.length) return null
  // stop is the one Tab stop: the selected tab, or the first when the active
  // key matches no tab, so the bar never drops out of the Tab order.
  const stop = tabs.some(t => t.key === active) ? active : tabs[0].key
  const onKeyDown = event => {
    if (!KEYS.includes(event.key)) return
    event.preventDefault()
    const buttons = [...event.currentTarget.querySelectorAll('[role="tab"]')]
    const from = Math.max(0, tabs.findIndex(t => t.key === active))
    const to = nextIndex(event.key, from, tabs.length)
    buttons[to]?.focus()
    if (tabs[to].key !== active) onSelect(tabs[to].key)
  }
  return (
    <div role="tablist" aria-label={label} className={className}
      onKeyDown={onKeyDown} {...rest}>
      {tabs.map(t => {
        const selected = t.key === active
        const ids = tabIds(id, t.key)
        return (
          <button key={t.key} type="button" role="tab" id={ids.tab}
            aria-selected={selected} aria-controls={selected ? ids.panel : undefined}
            tabIndex={t.key === stop ? 0 : -1} data-testid={t.testId}
            onClick={() => onSelect(t.key)} className={tabClassName}
            style={tabStyle?.(selected)}>
            {t.label}
          </button>
        )
      })}
    </div>
  )
}

const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]), '
  + 'select:not([disabled]), textarea:not([disabled]), summary, '
  + '[tabindex]:not([tabindex="-1"])'

// useStopWhenEmpty keeps the panel's tabindex="0" exactly while no element
// inside it can take focus, following the content as it loads or changes.
function useStopWhenEmpty(ref) {
  useEffect(() => {
    const el = ref.current
    if (!el) return undefined
    const update = () => {
      const empty = !el.querySelector(FOCUSABLE)
      if (empty && el.getAttribute('tabindex') !== '0') el.setAttribute('tabindex', '0')
      if (!empty && el.hasAttribute('tabindex')) el.removeAttribute('tabindex')
    }
    update()
    const observer = new MutationObserver(update)
    observer.observe(el, { childList: true, subtree: true, attributes: true,
      attributeFilter: ['disabled', 'href', 'tabindex'] })
    return () => observer.disconnect()
  }, [ref])
}

// TabPanel wraps the content of the selected tab of the bar with this id.
export function TabPanel({ id, active, className, children }) {
  const ids = tabIds(id, active)
  const ref = useRef(null)
  useStopWhenEmpty(ref)
  return (
    <div ref={ref} role="tabpanel" id={ids.panel} aria-labelledby={ids.tab}
      className={className}>
      {children}
    </div>
  )
}
