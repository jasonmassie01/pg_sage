# Agent DB Identity And Ping Tokens Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add conservative agent identity records and deployment-bound ping tokens so agents can refresh only their own leases without broad API authority.

**Architecture:** Operators create agent identities and one-time-visible ping tokens. Tokens are stored as hashes, scoped to one deployment and the `ping` action, and validated by a narrow agent ping endpoint. This is not full enterprise IAM or workload federation.

**Tech Stack:** Go, pgx, crypto/rand, sha256, net/http.

---

### Task 1: Store Contract

**Files:**
- Modify: `sidecar/internal/agentdb/types.go`
- Modify: `sidecar/internal/agentdb/schema.go`
- Create: `sidecar/internal/agentdb/identity.go`
- Create: `sidecar/internal/agentdb/identity_test.go`

- [ ] Write failing tests for identity upsert/list, ping token creation, wrong-token rejection, and token-backed ping.
- [ ] Add identity and token schema.
- [ ] Implement hash-only token storage and validation.
- [ ] Run targeted store tests.

### Task 2: API Contract

**Files:**
- Modify: `sidecar/internal/api/agent_db_handlers.go`
- Create: `sidecar/internal/api/agent_db_identity_handlers.go`
- Test: `sidecar/internal/api/agent_db_handlers_test.go`

- [ ] Write failing tests for identity endpoints, ping-token creation, and token-backed agent ping.
- [ ] Add routes for identities, ping tokens, and agent ping.
- [ ] Ensure agent ping does not list or mutate anything else.
- [ ] Run targeted API tests.

### Task 3: Verification

**Files:**
- Modify: `tasks/todo.md`

- [ ] Run targeted Go tests.
- [ ] Run full verification after implementation.
