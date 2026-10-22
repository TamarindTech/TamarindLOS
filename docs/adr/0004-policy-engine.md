# ADR-0004: Policy Engine

| | |
|---|---|
| **Status** | Proposed |
| **Date** | 2026-10-22 |
| **Decision owner** | Christopher Tun |
| **Related** | [Reference Architecture §7, §8](../architecture/02-reference-architecture.md), [ADR-0001](0001-core-language.md) |

## Context

The Tool Gateway evaluates policy on every tool invocation. Tamarind needs to express two different kinds of policy:

1. **Authorization:** who (human, agent or system), acting on whose behalf, may do what, up to which limits. Examples: approval authority by amount, maker-checker, tenant isolation, scope intersection for delegated agents.
2. **Business rules:** eligibility and knock-out rules, tool preconditions, document requirements and state-machine guards. Examples: "Business trading for at least 12 months", "No active sanctions hit", "Financial statements required above amount X".

Every evaluation must return the **policy version and matched rules** for the audit trail. Policies must be testable, versioned, and readable by risk and compliance staff, not just engineers.

## Decision drivers

- Embeddable in the Go core (ADR-0001) with no extra service required for the Lite topology.
- Fast enough to run on every tool call.
- Readable by non-engineers; analysable and testable.
- Returns explanations (which rules matched).
- Mature, open-source, with an active community.

## Options considered

### Option A — Open Policy Agent (OPA) with Rego
- **Pros:** CNCF-graduated, very mature, embeddable in Go, handles both authorization and general rules.
- **Cons:** Rego has a steep learning curve and is hard for risk and compliance staff to read; one engine for two different jobs.

### Option B — Cedar
- **Pros:** purpose-built authorization language (principal, action, resource, conditions); readable; designed for formal analysis; Go implementation (cedar-go) available.
- **Cons:** focused on authorization, so not a natural fit for eligibility rules or document requirements.

### Option C — Common Expression Language (CEL)
- **Pros:** simple, safe, fast expressions; Go implementation (cel-go) is widely used; natural for preconditions and eligibility rules.
- **Cons:** expressions only, with no built-in model for authorization policies, so roles and delegation would need custom code.

### Option D — Cedar for authorization + CEL for business rules
- **Pros:** right tool for each job; both embed in Go; both are readable.
- **Cons:** two policy languages for contributors to learn.

### Option E — Custom decision tables only
- **Pros:** most familiar format for business users.
- **Cons:** building and maintaining an engine; weaker guarantees and tooling.

## Decision (recommendation)

**Option D.**

- **Cedar** for authorization: roles, delegation, approval authority, maker-checker and tenant isolation.
- **CEL** for business rules: eligibility, knock-outs, preconditions, guards and document requirements.
- Later, a **decision-table format** (YAML) that compiles to CEL, so risk teams can maintain rules in a familiar shape.

## Consequences

- The gateway runs two evaluation steps on each call: Cedar authorization, then CEL preconditions and rules. Both results, with policy IDs and versions, go into the audit record.
- Policy packs contain Cedar policies, CEL rule sets and their test cases. CI runs the policy tests.
- If maintaining two languages proves burdensome, revisit Option A (OPA only).
