//go:build !cgo

package broker

// useScriptedParser gives b the script's reads: this build has no SQL
// parser, so without one every statement is refused before execution.
func useScriptedParser(b *Broker, script map[string]scripted) {
	b.parse = scriptedParser(script)
}
