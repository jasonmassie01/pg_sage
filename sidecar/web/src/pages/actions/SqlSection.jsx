import { SQLBlock } from '../../components/SQLBlock'

// SqlSection is a captioned SQL block in an expanded action row.
export function SqlSection({ title, sql }) {
  return (
    <div>
      <div className="text-xs font-medium mb-1"
        style={{ color: 'var(--text-secondary)' }}>
        {title}
      </div>
      <SQLBlock sql={sql} />
    </div>
  )
}
