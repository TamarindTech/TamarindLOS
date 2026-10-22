# Specifications

Specifications turn the [reference architecture](../architecture/02-reference-architecture.md) into contracts that code is built and generated from. Where a spec and a paper disagree, the spec wins.

| Spec | Covers | Machine-readable source |
|---|---|---|
| [03 — Tool Specification](03-tool-specification.md) | Tool definition format, invocation envelope, error codes, MCP and REST mapping, initial catalogue | [tools/](../../tools) |
| [04 — Data Model](04-data-model.md) | PostgreSQL schema, tenancy, money, append-only records, audit hash chain | [services/core/migrations/0001_init.sql](../../services/core/migrations/0001_init.sql) |
| [05 — SME Working-Capital Product](05-sme-working-capital.md) | Reference product, parties, documents, checks, state machine, rules, authorization | [policy-packs/baseline/sme-working-capital/](../../policy-packs/baseline/sme-working-capital) |

## Validation status

All machine-readable sources were validated when first published (2026-10-22):

- 31 tool definitions validate against [tool-definition.schema.json](../../tools/schema/tool-definition.schema.json), with no duplicate names.
- The migration applies cleanly on PostgreSQL 16. Tested guardrails: tenant isolation (row-level security), append-only decision records, human-only final decisions, maker-checker on approvals, and a mandatory reason for agent invocations.
- All CEL rules, state-machine guards and tool preconditions compile, and the four rule test cases pass.
- The Cedar policies parse and pass 12 authorization scenarios with the reference Cedar engine.

Automating these checks in CI is the next step.
