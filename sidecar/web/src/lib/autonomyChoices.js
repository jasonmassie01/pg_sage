// The four autonomy choices of the database form (ux-06b, decision D6):
// each names the stored trust_level and execution_mode pair it writes.

export const AUTONOMY_CHOICES = Object.freeze([
  { id: 'watch', label: 'Watch only', trust_level: 'observation', execution_mode: 'manual',
    note: 'Reports issues and evidence; changes nothing.' },
  { id: 'ask', label: 'Ask first', trust_level: 'advisory', execution_mode: 'approval',
    note: 'Every change waits for a person to approve it.' },
  { id: 'tell', label: 'Act, then tell', trust_level: 'advisory', execution_mode: 'auto',
    flag: 'tier3_safe',
    note: 'SAFE classes pg_sage has earned run on their own; you are told after.' },
  { id: 'window', label: 'Act in the window', trust_level: 'autonomous',
    execution_mode: 'auto', flag: 'tier3_moderate',
    note: 'Earned MODERATE classes run too, only inside the maintenance window.' },
])

// choiceFor names the choice a stored pair is, or null (Custom).
export function choiceFor(trustLevel, executionMode) {
  return AUTONOMY_CHOICES.find(c => c.trust_level === trustLevel
    && c.execution_mode === executionMode)?.id || null
}
