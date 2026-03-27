import { useState, Fragment } from 'react'
import { ChevronDown, ChevronRight } from 'lucide-react'

export function DataTable({ columns, rows, expandable, renderExpanded }) {
  const [expanded, setExpanded] = useState(null)

  return (
    <div className="rounded overflow-hidden"
      style={{ border: '1px solid var(--border)' }}>
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
          {rows.map((row, i) => (
            <Fragment key={i}>
              <tr
                className="cursor-pointer"
                style={{
                  borderTop: '1px solid var(--border)',
                  background: expanded === i
                    ? 'var(--bg-hover)' : 'transparent',
                }}
                onClick={() => expandable &&
                  setExpanded(expanded === i ? null : i)}>
                {expandable && (
                  <td className="p-2">
                    {expanded === i
                      ? <ChevronDown size={14}
                          style={{ color: 'var(--text-secondary)' }} />
                      : <ChevronRight size={14}
                          style={{ color: 'var(--text-secondary)' }} />}
                  </td>
                )}
                {columns.map(col => (
                  <td key={col.key} className="p-2"
                    style={{ color: 'var(--text-primary)' }}>
                    {col.render ? col.render(row) : row[col.key]}
                  </td>
                ))}
              </tr>
              {expandable && expanded === i && renderExpanded && (
                <tr>
                  <td colSpan={columns.length + 1} className="p-4"
                    style={{ background: 'var(--bg-hover)' }}>
                    {renderExpanded(row)}
                  </td>
                </tr>
              )}
            </Fragment>
          ))}
        </tbody>
      </table>
    </div>
  )
}
