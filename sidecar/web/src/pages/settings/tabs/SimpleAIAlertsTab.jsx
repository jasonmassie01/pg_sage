import { TokenBudgetBanner } from '../../../components/TokenBudgetBanner'
import { Field, ModelField, SectionHeading } from '../fields'

function AISection(props) {
  return (
    <div>
      <SectionHeading>AI Analysis</SectionHeading>
      <Field label="LLM Enabled" configKey="llm.enabled" type="toggle"
        help={'Enable AI-powered analysis for deeper insights and natural language'
          + ' health briefings.'}
        {...props} />
      <Field label="Endpoint URL" configKey="llm.endpoint" type="text"
        help={'The URL of your AI provider. Works with any OpenAI-compatible API'
          + ' (OpenAI, Gemini, Groq, Ollama, etc). Moving it to another host, scheme'
          + ' or port asks for the API key again.'}
        {...props} />
      <ModelField
        help={'Which AI model to use for analysis. Discover available models or'
          + ' type a name manually.'}
        {...props} />
      <Field label="API Key" configKey="llm.api_key" type="password"
        help={"Your AI provider's API key. Stored securely and never logged. It is"
          + ' only sent to the endpoint it was saved with: re-enter it, or clear it,'
          + ' when the endpoint moves.'}
        {...props} />
    </div>
  )
}

export function SimpleAIAlertsTab(props) {
  return (
    <div className="space-y-6">
      <TokenBudgetBanner canReset />
      <AISection {...props} />
      <div>
        <SectionHeading>Alerting</SectionHeading>
        <Field label="Alerting Enabled" configKey="alerting.enabled" type="toggle"
          help={'Turn on notifications so pg_sage can alert you when it finds'
            + ' issues or takes actions.'}
          {...props} />
        <Field label="Slack Webhook URL" configKey="alerting.slack_webhook_url" type="text"
          help={'Paste your Slack incoming webhook URL to receive alerts in a Slack'
            + ' channel.'}
          {...props} />
        <Field label="Check Interval (seconds)" configKey="alerting.check_interval_seconds"
          help={'How often pg_sage checks for new alerts to send. Lower values mean'
            + ' faster notifications but more processing.'}
          {...props} />
      </div>
    </div>
  )
}
