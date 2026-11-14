import { Field, SectionHeading } from '../fields'

export function CollectorTab(props) {
  return (
    <div>
      <SectionHeading>Collector</SectionHeading>
      <Field label="Interval (seconds)"
        configKey="collector.interval_seconds"
        help="How often to collect stats snapshots" {...props} />
      <Field label="Batch Size" configKey="collector.batch_size"
        help="Queries per collection batch" {...props} />
      <Field label="Max Queries" configKey="collector.max_queries"
        help="Maximum tracked queries" {...props} />
    </div>
  )
}
