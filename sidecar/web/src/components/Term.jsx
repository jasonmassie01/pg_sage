// Term renders a glossary word with its definition as a tooltip (ux-06a).
// The definitions come from docs/glossary.md through the generated
// glossary.json (go run ./cmd/gen_glossary/ from sidecar/), so the dashboard,
// the docs and the tooltips use one set of words. An unknown id renders its
// children as plain text and, in a dev build, warns once per id.
import * as Tooltip from '@radix-ui/react-tooltip'
import generated from '../generated/glossary.json'

const warned = new Set()

function warnUnknown(id, glossary) {
  if (!import.meta.env?.DEV || warned.has(id) || Object.keys(glossary).length === 0) return
  warned.add(id)
  console.warn(`Term: unknown glossary term "${id}"; add it to docs/glossary.md`)
}

export function Term({ id, children, glossary = generated, side = 'top' }) {
  const entry = glossary?.[id]
  if (!entry?.definition) {
    warnUnknown(id, glossary || {})
    return children ?? null
  }
  return (
    <Tooltip.Provider delayDuration={200}>
      <Tooltip.Root>
        <Tooltip.Trigger asChild>
          <span tabIndex={0} data-term={id}
            className="cursor-help border-b border-dotted border-gray-400">
            {children ?? entry.term}
          </span>
        </Tooltip.Trigger>
        <Tooltip.Portal>
          <Tooltip.Content side={side} sideOffset={6}
            className={'z-50 max-w-sm rounded-md px-3 py-2 text-xs shadow-lg '
              + 'bg-gray-900 text-gray-50 border border-gray-700'}>
            <div className="font-semibold mb-1">{entry.term}</div>
            <div className="whitespace-normal">{entry.definition}</div>
            <Tooltip.Arrow className="fill-gray-900" />
          </Tooltip.Content>
        </Tooltip.Portal>
      </Tooltip.Root>
    </Tooltip.Provider>
  )
}
