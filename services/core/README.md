# Tamarind Core Service

The core service is the **Tool Gateway** described in the [reference architecture](../../docs/architecture/02-reference-architecture.md). Every call, whether from a person, a partner system or an AI agent, goes through one pipeline:

```
authenticate → validate input (JSON Schema) → idempotency → authorize (Cedar, actor and represented user)
→ state and CEL preconditions → consent → agent autonomy (execute / ask a human / deny)
→ execute in one transaction → hash-chained events + outbox → audit record
```

It is written in Go ([ADR-0001](../../docs/adr/0001-core-language.md)) and loads the [tool catalogue](../../tools) and [policy pack](../../policy-packs/baseline/sme-working-capital) at start-up.

> **Phase 0 status.** This is a working foundation, not a product. Authentication uses static development tokens, and only the tools below are implemented. Do not use it with real data.

## Run it

### With Docker Compose

```bash
cd deploy/compose
docker compose up --build
```

This starts PostgreSQL 16, runs migrations and seeds two demo tenants, then serves on `http://localhost:8080`.

### Without Docker

You need Go 1.24+ and PostgreSQL 16.

```bash
cd services/core
export TAMARIND_TOOLS_DIR=../../tools
export TAMARIND_POLICY_PACK=../../policy-packs/baseline/sme-working-capital
export TAMARIND_PRINCIPALS=../../deploy/compose/principals.yaml

TAMARIND_APP_DB_PASSWORD=app-pw go run ./cmd/tamarind migrate --seed \
  --database-url 'postgres://postgres:postgres@localhost:5432/tamarind?sslmode=disable'

go run ./cmd/tamarind serve \
  --database-url 'postgres://tamarind_app:app-pw@localhost:5432/tamarind?sslmode=disable'
```

## Commands

| Command | Purpose |
|---|---|
| `tamarind validate` | Load the catalogue and policy pack, cross-check them, run the rule test cases. Used in CI. |
| `tamarind migrate [--seed]` | Apply migrations as an administrator; set the `tamarind_app` password from `TAMARIND_APP_DB_PASSWORD`; optionally seed tenants and products. |
| `tamarind serve` | Run the REST and MCP gateway. Connects as `tamarind_app`, so row-level security always applies. |
| `tamarind verify-audit --tenant demo` | Recompute the tenant's event hash chain and report whether it is intact. |

## Interfaces

| Endpoint | Description |
|---|---|
| `GET /healthz` | Liveness and database check |
| `GET /v1/tools` | Catalogue, with which tools are implemented and which the caller has scopes for |
| `POST /v1/tools/{name}` | Invoke a tool. Body: `{"input": {...}, "idempotency_key": "...", "reason": "...", "on_behalf_of": "...", "dry_run": false, "expected_state_version": 3}` |
| `POST /mcp` | MCP (Streamable HTTP, JSON responses): `initialize`, `ping`, `tools/list`, `tools/call` |

Responses always have the same shape: `status` (`ok`, `approval_required` or `error`), `result`, `approval_request_id`, `error {code, message, details}` and `invocation_id`. HTTP status codes follow the error code: 400 validation, 403 policy, 404 not found, 409 conflict, 422 precondition, 202 approval required.

### Implemented tools (Phase 0)

`application.create`, `application.get`, `application.next_actions`, `application.submit`, `application.withdraw`, `party.add`, `approval.respond`. Other catalogue tools return `NOT_IMPLEMENTED`.

## Try it

Development tokens are in [deploy/compose/principals.yaml](../../deploy/compose/principals.yaml).

```bash
API=http://localhost:8080/v1/tools
AGENT='Authorization: Bearer dev-agent-intake'

# The intake agent creates an application (agents must give a reason).
curl -s $API/application.create -H "$AGENT" -d '{
  "input": {"product_code": "SME-WC", "requested_amount_minor": 3000000, "currency": "USD", "requested_term_months": 12},
  "idempotency_key": "demo-1", "reason": "new enquiry from web chat"}'

# Ask what it may do next.
curl -s $API/application.next_actions -H "$AGENT" -d '{
  "input": {"application_id": "<id>"}, "reason": "planning next step"}'

# Withdrawing is autonomy L2 for agents: it becomes an approval request (HTTP 202).
curl -s $API/application.withdraw -H "$AGENT" -d '{
  "input": {"application_id": "<id>", "reason": "customer cancelled"},
  "idempotency_key": "demo-2", "reason": "customer asked to cancel", "on_behalf_of": "user:officer.ana"}'

# Ana cannot approve a request made on her behalf (maker-checker). Chai can.
curl -s $API/approval.respond -H 'Authorization: Bearer dev-officer-chai' -d '{
  "input": {"approval_request_id": "<approval id>", "decision": "approve"}, "idempotency_key": "demo-3"}'
```

### Connect an MCP client

Point any MCP client that supports Streamable HTTP at `http://localhost:8080/mcp` with an `Authorization: Bearer <token>` header. Tool names use underscores (`application_create`) because several clients reject dots. Call metadata goes in an `_envelope` argument:

```json
{"product_code": "SME-WC", "requested_amount_minor": 3000000, "currency": "USD", "requested_term_months": 12,
 "_envelope": {"reason": "new enquiry", "idempotency_key": "mcp-1"}}
```

## Tests

```bash
go test ./...                                            # unit tests (catalogue, policy, Cedar, autonomy)
TAMARIND_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable' \
  go test ./...                                          # plus end-to-end tests over REST and MCP
```

The end-to-end tests create a throwaway database and check, among other things: schema validation, CEL preconditions, the pack's party rules, optimistic concurrency, idempotent replay, agent autonomy and approval requests, maker-checker, re-authorization of approved calls, tenant isolation, MCP `tools/list` filtering, audit-chain verification, tamper detection and that the application role cannot edit events.

## Layout

```
cmd/tamarind/         CLI: validate, migrate, serve, verify-audit
internal/catalogue/   tool definitions and input validation
internal/policy/      policy pack: CEL rules and guards, Cedar authorization, autonomy
internal/gateway/     the invocation pipeline, handlers, events and audit chain
internal/api/         REST and MCP
internal/auth/        development principals (to be replaced by OIDC / OAuth 2.1)
internal/seed/        demo tenants and products
migrations/           SQL migrations (embedded in the binary)
```

## Known gaps

- Static tokens instead of OIDC / OAuth 2.1.
- Minimal in-house MCP server ([ADR-0005](../../docs/adr/0005-mcp-server-phase-0.md)).
- Documents, checks, decisions and offers are specified but not yet implemented.
- No outbox relay or agent runtime yet; events are written but not delivered.
