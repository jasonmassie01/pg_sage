// ConfigTooltip renders a Radix Tooltip whose content is sourced
// from the generated config_meta.json. Graceful degradation: if the
// configKey has no metadata entry, the children are returned as-is
// with no wrapper (CHECK-T05).
//
// Plan reference: reviews/archive/docs-internal/plan_v0.8.5.md §7.3.3.

import * as Tooltip from '@radix-ui/react-tooltip'
import configMeta from '../generated/config_meta.json'

// DOCS_BASE is the prefix applied to any meta.docs_url fragment for
// the "Read more →" link. Operators running their own docs server
// can override via Vite env.
const DOCS_BASE = import.meta.env?.VITE_PG_SAGE_DOCS_BASE || '/docs'

export function ConfigTooltip({ configKey, children, side = 'top' }) {
  const meta = configMeta[configKey]
  // Graceful degradation: unknown key → plain children, no wrapper.
  if (!meta || !meta.doc) {
    return children
  }

  const hasWarning = Boolean(meta.warning)
  const href = meta.docs_url ? `${DOCS_BASE}${meta.docs_url}` : null

  // Radix Provider is safe to nest — it no-ops if already wrapped
  // higher in the tree, so callers don't need to know.
  return (
    <Tooltip.Provider delayDuration={200}>
      <Tooltip.Root>
        <Tooltip.Trigger asChild>
          <span
            className="inline-flex items-center gap-1 cursor-help border-b border-dotted border-gray-400"
            data-config-key={configKey}
          >
            {children}
          </span>
        </Tooltip.Trigger>
        <Tooltip.Portal>
          <Tooltip.Content
            side={side}
            sideOffset={6}
            className={
              'z-50 max-w-sm rounded-md px-3 py-2 text-xs shadow-lg ' +
              (hasWarning
                ? 'bg-yellow-50 text-yellow-900 border border-yellow-300'
                : 'bg-gray-900 text-gray-50 border border-gray-700')
            }
          >
            <div className="font-semibold mb-1 break-all">
              {configKey}
            </div>
            <div className="mb-1 whitespace-normal">{meta.doc}</div>
            {hasWarning && (
              <div className="mt-2 pt-2 border-t border-yellow-300 font-semibold">
                ⚠ {meta.warning}
              </div>
            )}
            {meta.mode && meta.mode !== 'both' && (
              <div className="mt-1 text-[10px] uppercase opacity-70">
                {meta.mode}
              </div>
            )}
            {meta.secret && (
              <div className="mt-1 text-[10px] uppercase opacity-70">
                sensitive — not shown in UI
              </div>
            )}
            {href && (
              <div className="mt-2">
                <a
                  href={href}
                  className="underline text-blue-300 hover:text-blue-200"
                  target="_blank"
                  rel="noreferrer"
                >
                  Read more →
                </a>
              </div>
            )}
            <Tooltip.Arrow
              className={
                hasWarning ? 'fill-yellow-50' : 'fill-gray-900'
              }
            />
          </Tooltip.Content>
        </Tooltip.Portal>
      </Tooltip.Root>
    </Tooltip.Provider>
  )
}
