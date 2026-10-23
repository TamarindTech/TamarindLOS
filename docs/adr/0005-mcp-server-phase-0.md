# ADR-0005: MCP Server Implementation for Phase 0

| | |
|---|---|
| **Status** | Proposed |
| **Date** | 2026-10-23 |
| **Decision owner** | Christopher Tun |
| **Related** | [Spec 03 §6](../specs/03-tool-specification.md), [ADR-0001](0001-core-language.md) |

## Context

The Tool Gateway must expose tools to agents over the Model Context Protocol (MCP). Phase 0 needs only a small part of MCP: `initialize`, `ping`, `tools/list` and `tools/call` over Streamable HTTP, with bearer-token authentication. Every call must still go through the gateway's full pipeline (validation, Cedar, preconditions, autonomy, audit).

## Options considered

### Option A — Official MCP Go SDK
- **Pros:** full protocol coverage (sessions, streaming, resources, prompts, notifications); maintained by the MCP project.
- **Cons:** larger dependency surface while the protocol is still moving; the SDK's tool registration model has to be adapted so that tools are generated from the catalogue rather than declared in code.

### Option B — Minimal built-in JSON-RPC handler
- **Pros:** about 200 lines; tools generated directly from the catalogue; no extra dependencies; easy to reason about for security review.
- **Cons:** no server-initiated streams, resources, prompts or session management yet; we must track protocol changes ourselves.

## Decision (recommendation)

**Option B for Phase 0**, kept behind the `internal/api` package so it can be replaced. Move to the official Go SDK (Option A) in Phase 1, when MCP resources (`tamarind://applications/{id}/snapshot`) and OAuth 2.1 authorization are added.

## Consequences

- MCP tool names replace dots with underscores (`application.create` → `application_create`), because several MCP clients only accept `[a-zA-Z0-9_-]` in tool names. Spec 03 is updated accordingly.
- Call metadata (reason, idempotency key, delegation) is passed in an `_envelope` argument.
- Only implemented tools the caller holds scopes for appear in `tools/list`.
