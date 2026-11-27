import { useEffect } from 'react'
import { ShieldAlert } from 'lucide-react'
import { useAPI } from '../hooks/useAPI'

// SETTINGS_SAVED_EVENT is dispatched on window after a successful
// Settings save, so the banner re-reads the posture (security-08).
export const SETTINGS_SAVED_EVENT = 'sage:settings-saved'

const POSTURE_URL = '/api/v1/security/posture'

function message(posture) {
  if (posture.secrets_at_rest === 'key_missing') {
    return 'The generated encryption key (data_dir/encryption.key) is missing or ' +
      'was replaced, so sealed secrets cannot be opened and new ones are stored ' +
      'in plaintext. Restore the file from backup and restart pg_sage.'
  }
  const rows = posture.plaintext_rows || 0
  return `Secrets are stored in plaintext (${rows} stored now). Set ` +
    'encryption_key, or a persistent data_dir so pg_sage can generate a key, ' +
    'and restart pg_sage to seal them at rest.'
}

// SecretsPostureBanner warns admins, persistently, whenever stored
// secrets are not sealed. It reads the posture once (no polling) and again
// after a Settings save; a failed read shows nothing.
export function SecretsPostureBanner({ isAdmin }) {
  const { data: posture, error, refetch } = useAPI(isAdmin ? POSTURE_URL : null, 0)
  useEffect(() => {
    if (!isAdmin) return undefined
    window.addEventListener(SETTINGS_SAVED_EVENT, refetch)
    return () => window.removeEventListener(SETTINGS_SAVED_EVENT, refetch)
  }, [isAdmin, refetch])

  if (!isAdmin || error || !posture?.secrets_at_rest ||
    posture.secrets_at_rest === 'sealed') {
    return null
  }
  return (
    <div data-testid="secrets-posture-banner" role="alert"
      className="mb-4 rounded p-3 flex items-start gap-2 text-sm"
      style={{
        background: 'var(--bg-card)',
        border: '1px solid var(--red, #d33)',
        color: 'var(--text-primary)',
      }}>
      <ShieldAlert size={16} aria-hidden="true" style={{ color: 'var(--red, #d33)' }} />
      <span>{message(posture)}</span>
    </div>
  )
}
