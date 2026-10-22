# ADR-0002: First Reference Product and Market

| | |
|---|---|
| **Status** | Proposed: **decision needed from the owner** |
| **Date** | 2026-10-22 |
| **Decision owner** | Christopher Tun |
| **Related** | [Reference Architecture §8.2, §12, §19](../architecture/02-reference-architecture.md) |

## Context

Tamarind is designed to be jurisdiction-neutral: products, rules, document types and disclosures live in configurable **policy packs**. Even so, the first release needs one concrete **reference product** to build against end to end. That product sets the first state-machine variant, document types, checks, connectors, evaluation data and demo.

Two choices are bundled here: **which product** and **which market**.

## Decision drivers

- Shows off agent-native value (document-heavy, multi-step, real judgement).
- Achievable in Phase 1 without exotic integrations.
- Matches the needs of likely early adopters (smaller lenders, fintechs, microfinance institutions).
- Access to a **design partner**: a lender willing to review flows and supply anonymised sample cases.
- Availability of data providers (KYC, bureau, bank data) with sandbox access.

## Options considered — product

| Option | Pros | Cons |
|---|---|---|
| **Consumer personal loan** | Simple structure; huge market | High volume favours pure scoring; agent value is lower; heavy consumer-protection rules |
| **SME working-capital loan** | Document-heavy (financials, bank statements, registrations); multiple parties (directors, guarantors); strong showcase for agents and maker-checker | More complex party model; manual underwriting varies by lender |
| **Microfinance group loan** | High social impact; strong fit with Mifos / Fineract users | Group-lending workflows are specialised; less general-purpose |

## Options considered — market

| Option | Pros | Cons |
|---|---|---|
| **Jurisdiction-neutral baseline pack first** | No regulatory dependency; fastest to build; keeps the core honest about neutrality | Less realistic; a demo, not a deployable product |
| **One specific country first** | Realistic documents, rules and connectors; credible for pilots | Needs local regulatory and data-provider research; risk of baking local assumptions into the core |

Candidate countries should be chosen by **design-partner availability** and data-provider access.

## Decision (recommendation)

- **Product: SME working-capital loan (unsecured).** It shows the most agent-native value and exercises every part of the architecture.
- **Market: a jurisdiction-neutral baseline pack first, followed by the first country pack** as soon as a design partner is confirmed.

**Open question for the owner:** which country and which design partner to target for the first country pack.

## Consequences

- Phase 1 data model must support businesses, directors and guarantors from day one.
- Evaluation data starts as synthetic SME applications; anonymised real cases come from the design partner.
- Country packs are added as separate configuration, which tests the jurisdiction-neutral design early.
