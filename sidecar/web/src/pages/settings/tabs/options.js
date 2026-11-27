// The trust level and execution mode choices, shared by the simple
// Monitoring tab and the advanced Trust & Safety tab.
export const TRUST_OPTIONS = [
  { value: 'observation', label: 'Observation - Monitor only, no actions' },
  { value: 'advisory', label: 'Advisory - Safe actions only' },
  { value: 'autonomous', label: 'Autonomous - Safe + moderate actions' },
]

export const EXECUTION_OPTIONS = [
  { value: 'auto', label: 'Auto - Execute without approval' },
  { value: 'approval', label: 'Approval - Require manual approval' },
  { value: 'manual', label: 'Manual - All actions manual' },
]
