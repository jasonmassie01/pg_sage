function formatConfidence(value) {
  if (value === null || value === undefined || value === '') return null
  const numeric = Number(value)
  if (!Number.isFinite(numeric)) return null
  const percent = numeric <= 1 ? numeric * 100 : numeric
  return `${Math.round(percent)}%`
}

function instructionSummary(instructions) {
  if (!instructions) return ''
  if (typeof instructions === 'string') return instructions.trim()
  if (typeof instructions !== 'object') return ''
  const keys = [
    'summary',
    'instruction',
    'expected_change',
    'action',
    'rationale',
  ]
  const key = keys.find(name => typeof instructions[name] === 'string' &&
    instructions[name].trim() !== '')
  if (key) return instructions[key].trim()
  const value = Object.values(instructions).find(item =>
    typeof item === 'string' && item.trim() !== '')
  return value ? value.trim() : ''
}

export function RecommendationList({ recommendations }) {
  return (
    <div>
      <h3 className="mb-2 text-xs font-semibold"
        style={{ color: 'var(--text-secondary)' }}>
        Recommendations
      </h3>
      {recommendations.length === 0 ? (
        <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
          None
        </p>
      ) : recommendations.map(rec => (
        <div key={rec.recommendation_id} className="rounded border p-2"
          style={{ borderColor: 'var(--border)' }}>
          <div className="text-sm" style={{ color: 'var(--text-primary)' }}>
            {rec.title}
          </div>
          <div className="text-xs" style={{ color: 'var(--text-secondary)' }}>
            {rec.kind}: {rec.status}
          </div>
          <RecommendationMetadata rec={rec} />
        </div>
      ))}
    </div>
  )
}

function RecommendationMetadata({ rec }) {
  const confidence = formatConfidence(rec.confidence)
  const instruction = instructionSummary(rec.agent_instructions)
  const items = [
    rec.action_type ? `Action: ${rec.action_type}` : null,
    rec.action_risk ? `Risk: ${rec.action_risk}` : null,
    confidence ? `Confidence: ${confidence}` : null,
    instruction ? `Instruction: ${instruction}` : null,
  ].filter(Boolean)

  if (items.length === 0) return null

  return (
    <div className="mt-2 flex flex-wrap gap-1.5">
      {items.map(item => (
        <span key={item}
          className="rounded border px-2 py-1 text-xs"
          style={{
            borderColor: 'var(--border)',
            color: 'var(--text-secondary)',
          }}>
          {item}
        </span>
      ))}
    </div>
  )
}
