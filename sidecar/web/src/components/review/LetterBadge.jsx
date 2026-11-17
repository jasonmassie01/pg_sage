import { LETTER_STYLES, gradeBadge } from '../../lib/review'

// LetterBadge is one grade: its letter written out on a colour that keeps
// 4.5:1 contrast, or "Unknown: <reason>" in words. An invalid letter or a
// score outside 0-100 is "Unknown: invalid grade".
export function LetterBadge({ letter, score, reason, large = false }) {
  const badge = gradeBadge(letter, score, reason)
  const style = LETTER_STYLES[badge.unknown ? 'unknown' : badge.letter]
  const size = large ? 'text-2xl px-3 py-1' : 'text-sm px-2 py-0.5'
  return (
    <span data-testid="letter-badge" className={`inline-block rounded font-semibold ${size}`}
      aria-label={badge.unknown ? `Grade unknown: ${badge.reason}` : `Grade ${badge.letter}`}
      style={{ color: style.color, background: style.background, overflowWrap: 'anywhere' }}>
      {badge.unknown ? `Unknown: ${badge.reason}` : badge.letter}
    </span>
  )
}
