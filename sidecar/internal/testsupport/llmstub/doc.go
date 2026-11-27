// Package llmstub is a scripted OpenAI-compatible chat-completions server
// for tests that drive the real pg_sage binary or the real llm client
// without a provider. Each request is answered by the first Rule whose
// Match text appears in its prompt (or by the Default), in one of the
// scripted Modes, and is recorded so a test can assert what was asked.
package llmstub
