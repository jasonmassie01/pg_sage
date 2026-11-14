import { TokenBudgetBanner } from '../../../components/TokenBudgetBanner'
import { Field, ModelField, SectionHeading } from '../fields'

function ConnectionSection(props) {
  return (
    <div>
      <SectionHeading>LLM</SectionHeading>
      <Field label="Enabled" configKey="llm.enabled"
        type="toggle" {...props} />
      <Field label="Endpoint URL" configKey="llm.endpoint"
        type="text" {...props} />
      <Field label="API Key" configKey="llm.api_key"
        type="password" {...props} />
      <ModelField {...props} />
      <Field label="Timeout (seconds)"
        configKey="llm.timeout_seconds" {...props} />
      <Field label="Token Budget (daily)"
        configKey="llm.token_budget_daily" {...props} />
      <Field label="Context Budget (tokens)"
        configKey="llm.context_budget_tokens" {...props} />
    </div>
  )
}

export function LLMTab(props) {
  return (
    <div className="space-y-6">
      <TokenBudgetBanner canReset />
      <ConnectionSection {...props} />
      <div>
        <SectionHeading>Advisor</SectionHeading>
        <Field label="Advisor Enabled"
          configKey="advisor.enabled" type="toggle"
          help="LLM-powered config and query rewrite advisor"
          {...props} />
        <Field label="Advisor Interval (seconds)"
          configKey="advisor.interval_seconds"
          help="How often the advisor runs (min 5s)"
          {...props} />
      </div>
      <div>
        <SectionHeading>Index Optimizer</SectionHeading>
        <Field label="Optimizer Enabled"
          configKey="llm.optimizer.enabled" type="toggle"
          help="LLM-powered index recommendation engine"
          {...props} />
        <Field label="Min Query Calls"
          configKey="llm.optimizer.min_query_calls"
          help="Minimum query executions before optimizing"
          {...props} />
        <Field label="Max New Indexes Per Table"
          configKey="llm.optimizer.max_new_per_table"
          help="Cap on new indexes recommended per table"
          {...props} />
      </div>
    </div>
  )
}
