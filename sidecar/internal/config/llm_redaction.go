package config

// LLMRedactionOwner is the reconfiguration owner name of
// llm.redact_numeric_literals (llm.RedactionOwner).
const LLMRedactionOwner = "llm_redaction"

// DefaultLLMRedactNumericLiterals: long compared numbers are redacted
// before text reaches a model unless the operator turns it off.
const DefaultLLMRedactNumericLiterals = true
