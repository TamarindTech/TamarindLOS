# ADR-0001: Core Implementation Languages

| | |
|---|---|
| **Status** | Accepted (2026-10-22) |
| **Date** | 2026-10-22 |
| **Decision owner** | Christopher Tun |
| **Related** | [Reference Architecture §3, §9, §16](../architecture/02-reference-architecture.md) |

## Context

Tamarind needs languages for three parts of the system:

1. **Core and Tool Gateway:** domain model, state machine, policy enforcement, audit ledger, REST and MCP adapters. This is the safety-critical part that handles money-related state.
2. **Agent runtime:** orchestration, LLM gateway, context building and document intelligence. This part depends heavily on the AI ecosystem.
3. **User interfaces:** officer console and borrower portal.

The architecture already puts a clean boundary between the agent runtime and the core: agents call tools through the gateway over MCP, exactly like external agents. The two sides can therefore use different languages without breaking the design.

## Decision drivers

- **Correctness** of financial state and policy enforcement (strong typing, explicit errors, easy testing).
- **Lightweight deployment** (small footprint, simple to run on one server).
- **AI ecosystem** for the agent runtime (model SDKs, document extraction, evaluation tools).
- **Contributor pool** in fintech and AI.
- **MCP support:** official MCP SDKs exist for Python, TypeScript and Go.
- **Team familiarity:** the founding team works in Python, TypeScript and Go.

## Options considered

### Option A — Python everywhere (except the UI)
- **Pros:** one backend language; strongest AI ecosystem; fast to prototype; large contributor pool.
- **Cons:** weaker compile-time guarantees for the core; heavier runtime footprint; concurrency is less natural for the gateway and event relay.

### Option B — Go for core and gateway, Python for the agent runtime
- **Pros:** the core ships as a single static binary with a small footprint and strong typing; Python keeps full access to the AI ecosystem where it matters; the MCP boundary already separates them; good embedded policy libraries in Go (see ADR-0004).
- **Cons:** two backend toolchains; tool schemas need code generation for both; the Lite topology becomes two processes instead of one.

### Option C — TypeScript everywhere
- **Pros:** one language from backend to UI; good MCP and AI SDK support; large contributor pool.
- **Cons:** document-intelligence and evaluation tooling are less mature than in Python; less common for core-banking-grade backends.

### Option D — Go everywhere (except the UI)
- **Pros:** one backend language; smallest footprint.
- **Cons:** thinner AI and document-processing ecosystem; would push contributors to rebuild tooling that already exists in Python.

## Decision

**Option B.** Go for the core, Tool Gateway and adapters; Python for the agent runtime; TypeScript (React) for the officer console and borrower portal.

## Consequences

- Tool definitions (YAML + JSON Schema) are the single source of truth. Generate Go server stubs and Python client types from them.
- The Lite topology runs two application processes (core and agent runtime) plus PostgreSQL and object storage, still on one host with Docker Compose.
- The agent runtime holds **no privileged access**: it reaches data only through the gateway. This keeps the security boundary simple.
- Revisit if the Python runtime turns out to be thin enough to move into Go, or if contributor feedback strongly favours one language.
