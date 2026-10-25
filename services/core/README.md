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
| `POST /v1/tools/{name}` | Invoke a tool. Body: `{"input": {...}, "idempotency_key": "...", "reason": "...", "on_behalf_of": "...", "dry_run": false, "expected_state_version": 3, "run_id": "..."}` |
| `POST /mcp` | MCP (Streamable HTTP, JSON responses): `initialize`, `ping`, `tools/list`, `tools/call` |
| `PUT /v1/uploads/{token}` | Upload a file to a link from `document.upload_url`. The link is the credential (single use, 15 minutes); the file is hashed, its type checked against the declared MIME type and scanned for malware |
| `GET /v1/events?after=0&types=…&wait=25` | The tenant's domain events in order (scope `events:read`). With `wait`, long-polls until an event commits. Used by the agent runtime ([ADR-0006](../../docs/adr/0006-agent-event-delivery.md)) |

Responses always have the same shape: `status` (`ok`, `approval_required` or `error`), `result`, `approval_request_id`, `error {code, message, details}` and `invocation_id`. HTTP status codes follow the error code: 400 validation, 403 policy, 404 not found, 409 conflict, 422 precondition, 202 approval required.

### Implemented tools (Phase 0)

| Stage | Tools |
|---|---|
| Intake | `application.create`, `application.get`, `application.next_actions`, `application.submit`, `application.withdraw`, `party.add`, `consent.record` |
| Documents | `document.upload_url`, `document.attach`, `document.classify`, `document.extract`, `evidence.verify`, `document.request` |
| Verification | `kyc.run` (KYC for people, KYB for businesses), `sanctions.screen`, `credit.pull_report`, `bank.analyse_statements` |
| Decision | `policy.evaluate`, `decision.recommend`, `decision.approve`, `decision.decline` |
| Offer and booking | `offer.generate`, `offer.send`, `offer.accept`, `loan.handoff` |
| Work | `task.list`, `task.create`, `task.complete` (the shared work queue), `approval.list` (the approval queue, with the proposal and whether you may decide), `approval.respond` |
| Agents | `agent.run_start`, `agent.run_finish`, `agent.run_list` (what each agent did, call by call) |

`application.update`, `party.update` and `decision.refer` return `NOT_IMPLEMENTED` for now.

### Documents and evidence

Required documents come from the policy pack (some only above an amount, via CEL `required_when`). A document counts towards completeness once it is classified as the required type for the right party. Extraction turns fields into evidence with a confidence score. Agents may mark evidence verified only at or above the pack's `min_confidence_auto_verify` (0.90) and cannot correct values; people can, and the original value is kept. Extracted values stay out of the event log. Upload links never reach the invocation log.

Mock documents are small PDFs whose text names their type and fields:

```
%PDF-1.4
% TAMARIND-DOC: business_registration
registration_number: 0105561234567
legal_name: Mekong Trading Co.
```

A value ending in `?` is read with low confidence. A file containing the EICAR test string is rejected as malware. Files are stored on disk (`TAMARIND_BLOB_DIR`) behind a blob-store port meant for S3-compatible storage.

### Tasks and agent runs

People and agents share one work queue. A task is assigned to a principal or a role (`role:loan_officer`); `task.list` shows by default the open tasks assigned to you or to one of your roles, and only the assignee or the creator may complete one. Agents escalate by creating a task. `document.request` creates a task for the borrower (or the loan officers when there is no borrower login), keeps one open request per document and party, and the request closes itself when a document of that type is classified.

Every time an agent acts it opens a run (`agent.run_start`) and passes the run id in the envelope of each call (`run_id`), so `agent.run_list` with a `run_id` returns the run's tool calls in order, each with its reason, autonomy and outcome. The run records what woke the agent, how it ended (`succeeded`, `escalated`, `failed`, `cancelled`), its step count, token usage and cost when reported, and the task it handed to a person. Only the agent that opened a run can use or close it, an agent has at most one open run per application, and a run left open for more than 10 minutes (the agent crashed) is closed as failed when the next one starts. Run events are recorded on the run, so they do not change the application's `state_version`.

### The journey

```
draft --submit--> submitted --> intake --(parties + consents + classified documents)--> verification
verification --(all mandatory checks clear)--> underwriting --approve--> decisioned
decisioned --offer.send--> offered --offer.accept--> accepted --loan.handoff--> handed_off
verification --(a check needs review)--> referred      any review state --decline--> closed
```

Arrows without a tool name are automatic: after every change the gateway recomputes the snapshot (completeness, check summary, cash-flow metrics) and follows the pack's system transitions.

Who does what in the demo tenant:

| Principal | Token | Role in the journey |
|---|---|---|
| `user:officer.ana`, `user:officer.chai` | `dev-officer-ana`, `dev-officer-chai` | Create applications, review agent proposals, generate and send offers |
| `borrower:mekong` | `dev-borrower-mekong` | Sees only applications linked to it; records consent, accepts offers |
| `agent:intake` | `dev-agent-intake` | Creates applications and adds parties; asks for missing documents (L3) |
| `agent:documents` | `dev-agent-documents` | Classifies and extracts uploads; verifies confident evidence that matches the application (L3) |
| `agent:verification` | `dev-agent-verification` | Runs checks (L3) |
| `agent:underwriting` | `dev-agent-underwriting` | Evaluates policy; proposes recommendations (L2: a person must approve) |
| `user:approver.ben` | `dev-approver-ben` | Approves up to 25,000; declines |
| `user:approver.sara` | `dev-approver-sara` | Senior approver, up to 100,000 |
| `user:ops.dana` | `dev-ops-dana` | Hands accepted loans to core banking |

In Compose, the intake, document, verification and underwriting agents run on their own in the `agents` service ([services/agents](../agents/README.md)).

### Mock connectors

KYC/KYB, sanctions, credit bureau, bank data, core banking and notifications are ports (`internal/connectors`) with deterministic mock adapters, so the system runs without external accounts. Results are healthy by default. Put a marker in a party's legal name to exercise other paths: `SANCTION` (confirmed sanctions match), `FAILKYC` (identity fails), `DEFAULT` (serious bureau default), `THIN` (weak cash flow), `STARTUP` (7 months trading).

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

`TestDocumentPipeline` covers upload links (unknown, reused, oversized), file-type and malware rejection, classification, extraction, the agent confidence guardrail, human correction, document requests and that upload links are not logged. `TestEventFeed` checks ordering, type filters, scopes and that a long-polling reader wakes on commit. `TestLoanJourney` takes one loan from intake to booking (consent gates, six checks, policy, an agent proposal approved by a person, approval limits, offer, borrower acceptance, hand-off). `TestDeclinePath` follows a sanctions match to a referred case and a decline with an adverse-action event. `TestTasksAndAgentRuns` covers runs (one open per agent and application, calls linked to the run, no borrowing another agent's run, closed runs refuse changes, stale runs), escalation tasks, who may list and complete tasks, and document requests that close when the document is classified.

## Layout

```
cmd/tamarind/         CLI: validate, migrate, serve, verify-audit
internal/catalogue/   tool definitions and input validation
internal/policy/      policy pack: CEL rules and guards, Cedar authorization, autonomy
internal/gateway/     the invocation pipeline, application snapshot, handlers, events and audit chain
internal/connectors/  ports to external services, with mock adapters
internal/api/         REST and MCP
internal/auth/        development principals (to be replaced by OIDC / OAuth 2.1)
internal/seed/        demo tenants and products
migrations/           SQL migrations (embedded in the binary)
```

## Known gaps

- Static tokens instead of OIDC / OAuth 2.1.
- Minimal in-house MCP server ([ADR-0005](../../docs/adr/0005-mcp-server-phase-0.md)).
- Classification and extraction are mocks; bank-statement months and financial-statement years are not yet checked against the pack minimums.
- Connectors are mocks. Real adapters plug into the same ports.
- Pricing uses the pack's reference rate; risk grading is not implemented yet.
- No outbox relay yet: agents read events from the feed, but there are no outbound webhooks.
- Task due dates are recorded but nothing chases overdue tasks yet.
