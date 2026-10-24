# ADR-0006: How agents receive events in Phase 0

| | |
|---|---|
| **Status** | Proposed |
| **Date** | 2026-10-24 |
| **Decision owner** | Christopher Tun |
| **Related** | [Reference Architecture §9.2, §10.1](../architecture/02-reference-architecture.md), [Spec 03 §6](../specs/03-tool-specification.md) |

## Context

Agents wake on domain events (Reference Architecture §9.2). Events are written in the same PostgreSQL transaction as the state change. The Lite topology has PostgreSQL as its only stateful dependency, so a message broker is not an option for Phase 0. We need delivery that is ordered, survives restarts, wakes agents quickly and keeps the agent runtime outside the trust boundary.

## Options considered

- **A. Pull feed with long polling.** `GET /v1/events?after={seq}&wait={s}` returns the tenant's events in order. The request blocks until an event commits, using PostgreSQL `LISTEN/NOTIFY` (fired by `pg_notify` inside the writing transaction). Each subscriber keeps its own cursor.
- **B. Push (webhooks) from an outbox relay.** The core marks outbox rows as published and calls subscriber URLs. Needs retry, backoff and endpoint management, and the agent runtime must expose an HTTP endpoint.
- **C. A broker (NATS, Kafka).** Good at scale, but adds an operational dependency to the Lite topology.

## Decision

**Option A for Phase 0.** Agents read the feed with their own tokens (scope `events:read`), long-poll for up to 30 seconds and store their cursor locally. Handlers derive idempotency keys from the event id, so delivery is at least once with no duplicate effects.

Appends take a per-tenant advisory lock that is held until commit, so sequence order equals commit order within a tenant, and a cursor can't skip an event that commits late.

## Consequences

- No new infrastructure. Latency in tests is well under a second from commit to agent action.
- The feed is also the integration point for other subscribers (analytics, external agents) that hold `events:read`.
- Events carry identifiers, not personal data, so the feed exposes no PII. Subscribers read details through tools.
- Outbound webhooks (Option B) are still needed for partners. They will reuse the outbox table and can be added without changing agents.
- At higher scale, a broker (Option C) can sit behind the same feed contract.
