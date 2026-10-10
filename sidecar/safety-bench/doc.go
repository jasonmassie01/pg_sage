// Package safetybench is AgentSafetyBench v0: an open, reproducible bench
// that measures pg_sage's agent-governance controls the way PGIncidentBench
// (sre-bench) measures Sage SRE investigations (spec §11, G0-08).
//
// It runs three sections against real PostgreSQL (14-18):
//
//   - the read-only bypass regression corpus (RO-01..RO-16, incidents
//     §4.4), run against three read-only designs, to show which designs
//     hold under which bypass classes (readonly.go);
//   - posture scenarios (a Supabase-style exposed table, a permissive
//     policy, a definer function, PUBLIC CREATE, pgvector versions), run
//     through a PostureProvider adapter that the posture-detector
//     workstreams connect (posture.go);
//   - the incident-to-control mapping, scored against declared
//     expectations (SR-59): prevented, detected or out of scope, with
//     rows that need G1+ features recorded as "future release" so nothing
//     scores as if it passed in v0 (incidents.go).
//
// Scenarios are scripted and deterministic. There is no live-LLM arm in
// v0: the governed configurations are deterministic SQL paths. The bench
// never calls a cloud provider and holds no credentials. It runs only
// against the disposable fixture database it creates; it never touches a
// database it did not make.
package safetybench
