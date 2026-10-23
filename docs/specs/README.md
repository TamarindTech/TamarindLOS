# Specifications

Specifications turn the [reference architecture](../architecture/02-reference-architecture.md) into contracts that code is built and generated from. Where a spec and a paper disagree, the spec wins.

| Spec | Covers | Machine-readable source |
|---|---|---|
| [03 — Tool Specification](03-tool-specification.md) | Tool definition format, invocation envelope, error codes, MCP and REST mapping, initial catalogue | [tools/](../../tools) |
| [04 — Data Model](04-data-model.md) | PostgreSQL schema, tenancy, money, append-only records, audit hash chain | [services/core/migrations/0001_init.sql](../../services/core/migrations/0001_init.sql) |
| [05 — SME Working-Capital Product](05-sme-working-capital.md) | Reference product, parties, documents, checks, state machine, rules, authorization | [policy-packs/baseline/sme-working-capital/](../../policy-packs/baseline/sme-working-capital) |

## Validation status

All machine-readable sources are validated on every push and pull request by [CI](../../.github/workflows/ci.yml), using the same engines the service runs (cel-go, cedar-go, PostgreSQL 16):

- 31 tool definitions validate against [tool-definition.schema.json](../../tools/schema/tool-definition.schema.json), with no duplicate names.
- The migrations apply cleanly on PostgreSQL 16. Tested guardrails: tenant isolation (row-level security), append-only decision records, human-only final decisions, maker-checker on approvals, and a mandatory reason for agent invocations.
- All CEL rules, state-machine guards and tool preconditions compile, and the four rule test cases pass.
- The Cedar policies parse and pass 18 authorization scenarios.
- End-to-end tests run the gateway over REST and MCP against a real database.
