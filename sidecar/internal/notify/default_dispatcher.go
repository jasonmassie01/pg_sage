package notify

// NewDefaultDispatcher is the one place the built-in senders are
// registered, so the API router and the runtime dispatchers deliver to
// the same channel types under the same target policy (alerting-06).
// A nil logFn discards the dispatcher's log lines.
func NewDefaultDispatcher(
	store RuleStore, policy TargetPolicy, logFn func(string, string, ...any),
) *Dispatcher {
	if logFn == nil {
		logFn = func(string, string, ...any) {}
	}
	d := NewDispatcherWithStore(store, logFn)
	for _, s := range []Sender{
		NewSlackSenderWithPolicy(policy),
		NewEmailSenderWithPolicy(policy),
		NewPagerDutySender(),
		NewTelegramSenderWithPolicy(policy),
		NewWebhookSenderWithPolicy(policy),
		NewTeamsSenderWithPolicy(policy),
		NewOpsgenieSenderWithPolicy(policy),
		NewIncidentIOSenderWithPolicy(policy),
	} {
		d.RegisterSender(s)
	}
	return d
}
