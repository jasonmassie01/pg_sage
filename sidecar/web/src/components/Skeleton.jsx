// Skeleton is a shimmer placeholder. Callers pick width/height and
// shape (rect | circle | text). Respects prefers-reduced-motion
// via the global CSS rule in index.css.

const baseStyle = {
  background:
    'linear-gradient(90deg, var(--bg-card) 0%, ' +
    'var(--bg-hover) 50%, var(--bg-card) 100%)',
  backgroundSize: '200% 100%',
  animation: 'skeleton-shimmer 1.4s ease-in-out infinite',
  borderRadius: 4,
  display: 'inline-block',
}

export function Skeleton({
  width = '100%',
  height = 16,
  shape = 'rect',
  style = {},
  className = '',
  'data-testid': testId,
}) {
  const merged = { ...baseStyle, width, height, ...style }
  if (shape === 'circle') {
    merged.borderRadius = '50%'
  } else if (shape === 'text') {
    merged.borderRadius = 2
    merged.height = height || 12
  }
  return (
    <span
      role="status"
      aria-label="Loading"
      aria-busy="true"
      data-testid={testId || 'skeleton'}
      className={className}
      style={merged}
    />
  )
}

// SkeletonRow renders N same-width cells, useful for table rows.
export function SkeletonRow({ cols = 4, height = 14 }) {
  return (
    <div style={{ display: 'flex', gap: 12, padding: '8px 0' }}>
      {Array.from({ length: cols }).map((_, i) => (
        <Skeleton
          key={i}
          height={height}
          width={`${100 / cols}%`}
        />
      ))}
    </div>
  )
}

// SkeletonCard is a chunky placeholder sized like a dashboard tile.
export function SkeletonCard({ height = 120 }) {
  return (
    <div
      style={{
        background: 'var(--bg-card)',
        border: '1px solid var(--border)',
        borderRadius: 8,
        padding: 16,
        height,
        display: 'flex',
        flexDirection: 'column',
        gap: 10,
      }}
    >
      <Skeleton width="40%" height={12} />
      <Skeleton width="70%" height={22} />
      <Skeleton width="55%" height={10} />
    </div>
  )
}
