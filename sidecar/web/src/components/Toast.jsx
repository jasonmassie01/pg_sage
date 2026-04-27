import {
  createContext,
  useContext,
  useState,
  useCallback,
  useEffect,
} from 'react'
import { CheckCircle2, AlertCircle, XCircle, Info, X } from 'lucide-react'

const ToastContext = createContext(null)

// useToast exposes { success, error, info, warning } helpers.
// Falls back to a no-op shim when rendered outside the provider.
export function useToast() {
  const ctx = useContext(ToastContext)
  if (!ctx) {
    return {
      push: () => {},
      success: () => {},
      error: () => {},
      info: () => {},
      warning: () => {},
    }
  }
  return ctx
}

let nextId = 1

export function ToastProvider({ children }) {
  const [toasts, setToasts] = useState([])

  const dismiss = useCallback((id) => {
    setToasts(t => t.filter(x => x.id !== id))
  }, [])

  const push = useCallback((toast) => {
    const id = nextId++
    const duration = toast.duration ?? 4000
    setToasts(t => [...t, { ...toast, id }])
    if (duration > 0) {
      setTimeout(() => dismiss(id), duration)
    }
    return id
  }, [dismiss])

  const value = {
    push,
    success: (message, opts = {}) =>
      push({ ...opts, message, variant: 'success' }),
    error: (message, opts = {}) =>
      push({ ...opts, message, variant: 'error' }),
    info: (message, opts = {}) =>
      push({ ...opts, message, variant: 'info' }),
    warning: (message, opts = {}) =>
      push({ ...opts, message, variant: 'warning' }),
    dismiss,
  }

  return (
    <ToastContext.Provider value={value}>
      {children}
      <ToastViewport toasts={toasts} onDismiss={dismiss} />
    </ToastContext.Provider>
  )
}

function ToastViewport({ toasts, onDismiss }) {
  return (
    <div
      role="region"
      aria-live="polite"
      aria-label="Notifications"
      className="fixed z-50 flex flex-col gap-2"
      style={{
        right: '1rem',
        bottom: '1rem',
        maxWidth: 'min(420px, calc(100vw - 2rem))',
        pointerEvents: 'none',
      }}
    >
      {toasts.map(t => (
        <ToastItem key={t.id} toast={t} onDismiss={onDismiss} />
      ))}
    </div>
  )
}

const variantStyles = {
  success: {
    bg: '#0f2a1f',
    border: '#34d399',
    Icon: CheckCircle2,
    iconColor: '#34d399',
  },
  error: {
    bg: '#2a0f14',
    border: '#ef4444',
    Icon: XCircle,
    iconColor: '#ef4444',
  },
  warning: {
    bg: '#2a220f',
    border: '#fbbf24',
    Icon: AlertCircle,
    iconColor: '#fbbf24',
  },
  info: {
    bg: '#0f1d2a',
    border: '#4a9eff',
    Icon: Info,
    iconColor: '#4a9eff',
  },
}

function ToastItem({ toast, onDismiss }) {
  const style = variantStyles[toast.variant] || variantStyles.info
  const { Icon } = style
  const [entered, setEntered] = useState(false)
  useEffect(() => {
    const id = requestAnimationFrame(() => setEntered(true))
    return () => cancelAnimationFrame(id)
  }, [])
  return (
    <div
      role="status"
      data-testid="toast"
      data-variant={toast.variant}
      className="flex items-start gap-3 rounded px-3 py-2"
      style={{
        background: style.bg,
        border: `1px solid ${style.border}`,
        color: 'var(--text-primary)',
        pointerEvents: 'auto',
        boxShadow: '0 4px 12px rgba(0,0,0,0.4)',
        transform: entered ? 'translateX(0)' : 'translateX(calc(100% + 1.5rem))',
        transition: 'transform 180ms ease',
      }}
    >
      <Icon size={18} style={{ color: style.iconColor, flexShrink: 0, marginTop: 2 }} />
      <div style={{ flex: 1, fontSize: 14, lineHeight: 1.4 }}>
        {toast.title && (
          <div style={{ fontWeight: 600, marginBottom: 2 }}>
            {toast.title}
          </div>
        )}
        <div>{toast.message}</div>
      </div>
      <button
        type="button"
        onClick={() => onDismiss(toast.id)}
        aria-label="Dismiss notification"
        className="rounded hover:bg-white/10"
        style={{
          background: 'transparent',
          border: 0,
          color: 'var(--text-secondary)',
          cursor: 'pointer',
          padding: 2,
        }}
      >
        <X size={16} />
      </button>
    </div>
  )
}
