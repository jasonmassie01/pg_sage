package executor

// RuntimeSettings is a read-only snapshot of the per-database wiring a
// runtime constructor installed on an executor. Every deployment mode must
// produce the same settings for an equivalent database.
type RuntimeSettings struct {
	DatabaseName     string
	Provider         string
	TrustLevel       string
	ExecutionMode    string
	ExecutorEnabled  bool
	PolicyGate       bool
	ManagedConfig    bool
	AnalyzeSemaphore bool
	ActionStore      bool
	Dispatcher       bool
	Justifier        bool
	PostDDLHook      bool
}

// RuntimeSettings reports the executor's current per-database wiring.
func (e *Executor) RuntimeSettings() RuntimeSettings {
	if e == nil {
		return RuntimeSettings{}
	}
	cfg, mode, enabled := e.policySnapshot()
	settings := RuntimeSettings{
		DatabaseName: e.databaseName, ExecutionMode: mode,
		ExecutorEnabled: enabled, AnalyzeSemaphore: e.analyzeSem != nil,
		ActionStore: e.actionStore != nil, Dispatcher: e.dispatcher != nil,
		Justifier: e.justifier != nil,
	}
	if cfg != nil {
		settings.Provider = cfg.CloudEnvironment
		settings.TrustLevel = cfg.Trust.Level
	}
	e.policyMu.RLock()
	settings.PolicyGate = e.policyGate != nil
	settings.ManagedConfig = e.managedConfig != nil
	e.policyMu.RUnlock()
	e.postDDLMu.RLock()
	settings.PostDDLHook = e.postDDLHook != nil
	e.postDDLMu.RUnlock()
	return settings
}
