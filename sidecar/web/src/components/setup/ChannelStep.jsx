import { useState } from 'react'
import { ChannelForm } from '../../pages/notifications/ChannelForm'
import { CHANNELS_URL } from '../../pages/notifications/channelConfig'
import { channelTestMessage } from './wizardState'
import { Notice, PrimaryButton, SkipButton, postJSON } from './parts'

// ChannelStep is the setup wizard's step 3 (ux-04b): one notification
// channel through the Notifications page's own form, then Send test
// through the channel test route; only a delivered test finishes it.

function useChannelTest(onDone) {
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState(null)
  async function send(channel) {
    setBusy(true)
    setResult(null)
    try {
      const res = await postJSON(`${CHANNELS_URL}/${channel.id}/test`)
      if (!res.ok) {
        setResult({ ok: false, text: channelTestMessage(res.status, res.body?.error || '') })
        return
      }
      setResult({ ok: true, text: `Test notification sent to ${channel.name}` })
      onDone('channel')
    } catch (err) {
      setResult({ ok: false, text: `Could not reach pg_sage to send the test (${err.message})` })
    } finally {
      setBusy(false)
    }
  }
  return { busy, result, send }
}

function SendTest({ channel, test }) {
  return (
    <div className="flex items-center gap-3">
      <PrimaryButton testId="setup-channel-test" disabled={test.busy}
        onClick={() => test.send(channel)}>
        {test.busy ? 'Sending...' : `Send test to ${channel.name}`}
      </PrimaryButton>
      {test.result && (
        <Notice testId="setup-channel-result" ok={test.result.ok}>{test.result.text}</Notice>
      )}
    </div>
  )
}

export function ChannelStep({ configured, onDone, onSkip }) {
  const [channel, setChannel] = useState(null)
  const test = useChannelTest(onDone)
  return (
    <div className="space-y-3">
      <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
        Where should pg_sage reach you with issues and approvals?
      </p>
      {configured && !channel && (
        <div className="flex items-center gap-3">
          <p className="text-sm">A notification channel is configured.</p>
          <PrimaryButton testId="setup-next" onClick={() => onDone('channel')}>
            Next
          </PrimaryButton>
        </div>
      )}
      {channel
        ? <SendTest channel={channel} test={test} />
        : <ChannelForm onSaved={setChannel} />}
      <SkipButton step="channel" onSkip={onSkip} />
    </div>
  )
}
