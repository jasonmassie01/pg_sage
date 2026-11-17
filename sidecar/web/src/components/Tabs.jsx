// Tabs.jsx is the dashboard's one tab bar (ux-05d): a named tablist of
// tabs with a roving tab stop, Arrow Left and Right (wrapping), Home and
// End, and selection that follows focus; TabPanel is the region the
// selected tab controls. Pages keep their own look through className,
// tabClassName and tabStyle.

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
  const onKeyDown = event => {
    if (!KEYS.includes(event.key)) return
    event.preventDefault()
    const buttons = [...event.currentTarget.querySelectorAll('[role="tab"]')]
    const from = Math.max(0, tabs.findIndex(t => t.key === active))
    const to = nextIndex(event.key, from, tabs.length)
    buttons[to]?.focus()
    if (to !== from) onSelect(tabs[to].key)
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
            tabIndex={selected ? 0 : -1} data-testid={t.testId}
            onClick={() => onSelect(t.key)} className={tabClassName}
            style={tabStyle?.(selected)}>
            {t.label}
          </button>
        )
      })}
    </div>
  )
}

// TabPanel wraps the content of the selected tab of the bar with this id.
export function TabPanel({ id, active, className, children }) {
  const ids = tabIds(id, active)
  return (
    <div role="tabpanel" id={ids.panel} aria-labelledby={ids.tab} className={className}>
      {children}
    </div>
  )
}
