import { useState, Fragment, useRef, useEffect } from 'react'
import { ChevronDown, ChevronRight } from 'lucide-react'

function useOverflow(ref) {
  const [overflowing, setOverflowing] = useState(false)
  useEffect(() => {
    const el = ref.current
    if (!el) return
    const check = () => {
      setOverflowing(el.scrollWidth > el.clientWidth)
    }
    check()
    const ro = typeof ResizeObserver !== 'undefined'
      ? new ResizeObserver(check) : null
    if (ro) ro.observe(el)
    window.addEventListener('resize', check)
    return () => {
      if (ro) ro.disconnect()
      window.removeEventListener('resize', check)
    }
  }, [ref])
  return overflowing
}

// rowKey: (row, index) => string | number. Defaults to row.id, then
// row.finding_id, then the array index. Using a stable key prevents
// React from reusing an expanded row after a re-sort/filter refresh,
// which previously caused the drawer to open over the wrong finding.
export function DataTable({
  columns, rows, expandable, renderExpanded, rowKey, ...rest
}) {
  const [expanded, setExpanded] = useState(null)
  const scrollRef = useRef(null)
  const overflowing = useOverflow(scrollRef)

  const keyFor = (row, i) => {
    if (rowKey) return rowKey(row, i)
    if (row && row.id != null) return row.id
    if (row && row.finding_id != null) return row.finding_id
    return i
  }

  return (
    <div className="rounded relative"
      {...rest}
      style={{ border: '1px solid var(--border)' }}>
      {overflowing && (
        <div
          data-testid="scroll-hint"
          className="md:hidden absolute top-1 right-2 text-xs
            pointer-events-none px-1.5 py-0.5 rounded z-10"
          style={{
            background: 'var(--bg-hover)',
            color: 'var(--text-secondary)',
          }}>
          {'\u2190 scroll \u2192'}
        </div>
      )}
      <div ref={scrollRef} className="overflow-x-auto">
      <table className="w-full text-sm">
        <thead>
          <tr style={{ background: 'var(--bg-card)' }}>
            {expandable && <th className="w-8 p-2" />}
            {columns.map(col => (
              <th key={col.key} className="p-2 text-left font-medium"
                style={{ color: 'var(--text-secondary)' }}>
                {col.label}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((row, i) => {
            const k = keyFor(row, i)
            const isOpen = expanded === k
            return (
            <Fragment key={k}>
              <tr
                data-row-key={String(k)}
                className={expandable ? 'cursor-pointer' : ''}
                style={{
                  borderTop: '1px solid var(--border)',
                  background: isOpen
                    ? 'var(--bg-hover)' : 'transparent',
                }}
                onClick={() => expandable &&
                  setExpanded(isOpen ? null : k)}>
                {expandable && (
                  <td className="p-2">
                    <button
                      type="button"
                      aria-label={isOpen ? 'Collapse row' : 'Expand row'}
                      aria-expanded={isOpen}
                      data-testid="row-expander"
                      className="p-1 rounded"
                      onClick={e => {
                        e.stopPropagation()
                        setExpanded(isOpen ? null : k)
                      }}
                      onKeyDown={e => {
                        if (e.key !== 'Enter' && e.key !== ' ') return
                        e.stopPropagation()
                        e.preventDefault()
                        setExpanded(isOpen ? null : k)
                      }}
                      style={{ color: 'var(--text-secondary)' }}>
                      {isOpen
                        ? <ChevronDown size={14} />
                        : <ChevronRight size={14} />}
                    </button>
                  </td>
                )}
                {columns.map(col => (
                  <td key={col.key} className="p-2"
                    style={{ color: 'var(--text-primary)' }}>
                    {col.render ? col.render(row) : row[col.key]}
                  </td>
                ))}
              </tr>
              {expandable && isOpen && renderExpanded && (
                <tr>
                  <td colSpan={columns.length + 1} className="p-4"
                    style={{ background: 'var(--bg-hover)' }}>
                    {renderExpanded(row)}
                  </td>
                </tr>
              )}
            </Fragment>
            )
          })}
        </tbody>
      </table>
      </div>
    </div>
  )
}
