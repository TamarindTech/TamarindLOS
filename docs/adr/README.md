# Architecture Decision Records

This folder records the significant technical decisions for Tamarind LOS, using lightweight **Architecture Decision Records (ADRs)**. Each ADR captures the context, the options considered, the decision and its consequences, so future contributors understand *why* the system is the way it is.

## Status values

| Status | Meaning |
|---|---|
| **Proposed** | Drafted and open for discussion. Not yet binding. |
| **Accepted** | Agreed. Implementation should follow it. |
| **Superseded** | Replaced by a later ADR (linked). |
| **Rejected** | Considered and declined. Kept for the record. |

## How to propose one

Copy [0000-template.md](0000-template.md) to the next free number, fill it in, set the status to **Proposed**, and open a pull request. See [CONTRIBUTING.md](../../CONTRIBUTING.md) for details.

## Index

| ADR | Title | Status | Decision owner |
|---|---|---|---|
| [0001](0001-core-language.md) | Core implementation languages: Go core, Python agents, TypeScript UI | Accepted | Christopher Tun |
| [0002](0002-first-reference-product-and-market.md) | First reference product (SME working capital) and market | Product accepted · country pending | Christopher Tun |
| [0003](0003-licence.md) | Project licence: Apache-2.0 | Accepted | Christopher Tun |
| [0004](0004-policy-engine.md) | Policy engine: Cedar + CEL | Accepted | Christopher Tun |

### Planned

- Workflow engine: built-in PostgreSQL state machine vs. Temporal
- Default model profile and minimum self-hosted model support
- Event log and audit-chain design
- Tool definition format and code generation
