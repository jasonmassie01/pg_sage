import { useState } from 'react'
import { useAPI } from '../hooks/useAPI'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'
import { ShieldAlert, Play } from 'lucide-react'

export function SettingsPage({ database }) {
  const { data, loading, error, refetch } = useAPI('/api/v1/config')
  const [stopping, setStopping] = useState(false)

  if (loading) return <LoadingSpinner />
  if (error) return <ErrorBanner message={error} onRetry={refetch} />

  const emergencyStop = async () => {
    setStopping(true)
    const dbParam = database && database !== 'all'
      ? `?database=${database}` : ''
    try {
      await fetch(`/api/v1/emergency-stop${dbParam}`, { method: 'POST' })
    } finally {
      setStopping(false)
      refetch()
    }
  }

  const resume = async () => {
    const dbParam = database && database !== 'all'
      ? `?database=${database}` : ''
    await fetch(`/api/v1/resume${dbParam}`, { method: 'POST' })
    refetch()
  }

  return (
    <div className="space-y-6 max-w-2xl">
      <div className="rounded p-4"
        style={{
          background: 'var(--bg-card)',
          border: '1px solid var(--border)',
        }}>
        <h2 className="text-sm font-medium mb-3"
          style={{ color: 'var(--text-secondary)' }}>
          Configuration
        </h2>
        <pre className="text-xs overflow-auto"
          style={{ color: 'var(--text-secondary)' }}>
          {JSON.stringify(data, null, 2)}
        </pre>
      </div>

      <div className="rounded p-4"
        style={{
          background: 'var(--bg-card)',
          border: '1px solid var(--border)',
        }}>
        <h2 className="text-sm font-medium mb-3"
          style={{ color: 'var(--text-secondary)' }}>
          Emergency Controls
        </h2>
        <div className="flex gap-3">
          <button onClick={emergencyStop} disabled={stopping}
            className="flex items-center gap-2 px-4 py-2 rounded
              text-sm font-medium"
            style={{
              background: '#3b1111',
              color: 'var(--red)',
              border: '1px solid var(--red)',
            }}>
            <ShieldAlert size={16} />
            {stopping ? 'Stopping...' : 'Emergency Stop'}
          </button>
          <button onClick={resume}
            className="flex items-center gap-2 px-4 py-2 rounded
              text-sm font-medium"
            style={{
              background: '#0f2640',
              color: 'var(--green)',
              border: '1px solid var(--green)',
            }}>
            <Play size={16} />
            Resume
          </button>
        </div>
      </div>
    </div>
  )
}
