import { Field, SectionHeading } from '../fields'

export function AnalyzerTab(props) {
  return (
    <div>
      <SectionHeading>Analyzer</SectionHeading>
      <Field label="Interval (seconds)"
        configKey="analyzer.interval_seconds" {...props} />
      <Field label="Slow Query Threshold (ms)"
        configKey="analyzer.slow_query_threshold_ms" {...props} />
      <Field label="Seq Scan Min Rows"
        configKey="analyzer.seq_scan_min_rows" {...props} />
      <Field label="Unused Index Window (days)"
        configKey="analyzer.unused_index_window_days" {...props} />
      <Field label="Index Bloat Threshold (%)"
        configKey="analyzer.index_bloat_threshold_pct" {...props} />
      <Field label="Table Bloat Dead Tuple (%)"
        configKey="analyzer.table_bloat_dead_tuple_pct" {...props} />
      <Field label="Regression Threshold (%)"
        configKey="analyzer.regression_threshold_pct" {...props} />
      <Field label="Cache Hit Ratio Warning"
        configKey="analyzer.cache_hit_ratio_warning"
        type="float" help="0.0 to 1.0" {...props} />
    </div>
  )
}
