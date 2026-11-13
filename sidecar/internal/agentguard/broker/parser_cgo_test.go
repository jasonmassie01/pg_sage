//go:build cgo

package broker

// useScriptedParser keeps the C parser: with cgo every test statement is
// read by the real parser, and the script only documents the expectation.
func useScriptedParser(_ *Broker, _ map[string]scripted) {}
