# ADR-0003: Project Licence

| | |
|---|---|
| **Status** | Accepted (2026-10-22) |
| **Date** | 2026-10-22 |
| **Decision owner** | Christopher Tun |
| **Related** | [LICENSE](../../LICENSE), [NOTICE](../../NOTICE) |

> This ADR summarises common trade-offs between open-source licences. It is not legal advice; confirm the final choice with a lawyer.

## Context

The repository was created with the **MIT License**. Before outside contributions arrive, the project should confirm the licence on purpose. Changing it is easy while there is a single copyright holder, and much harder once there are many contributors.

Tamarind's likely adopters include banks, microfinance institutions and fintechs, whose legal teams review open-source licences carefully. Tamarind is also designed to integrate with Apache Fineract, which is licensed under Apache-2.0.

## Decision drivers

- **Adoption** by regulated financial institutions.
- **Patent protection** for users and contributors.
- **Compatibility** with the ecosystem Tamarind integrates with.
- **Sustainability:** whether to stop closed, hosted forks that don't contribute back.
- **Simplicity** for contributors.

## Options considered

### Option A — Keep MIT
- **Pros:** maximally permissive and simple; widely accepted.
- **Cons:** no explicit patent grant; offers no protection against closed forks.

### Option B — Apache-2.0
- **Pros:** permissive and widely accepted by enterprises and banks; includes an **explicit patent licence** and patent-retaliation clause; same licence as Apache Fineract; clear contribution terms.
- **Cons:** slightly more obligations than MIT (notices, stating changes); still allows closed forks.

### Option C — AGPL-3.0
- **Pros:** strong copyleft, including for software offered over a network; prevents closed hosted forks.
- **Cons:** many banks and enterprises restrict or forbid AGPL software, which would slow adoption; makes commercial embedding harder.

### Option D — AGPL-3.0 plus a commercial licence (dual licensing)
- **Pros:** open community version, plus revenue from organisations that need other terms.
- **Cons:** requires contributor licence agreements (CLAs) to keep relicensing rights; can discourage contributors; more governance overhead.

## Decision

**Option B: Apache-2.0.** The project was relicensed from MIT on 2026-10-22, while there was still a single copyright holder. Commits require **Developer Certificate of Origin (DCO)** sign-off on commits (see [CONTRIBUTING.md](../../CONTRIBUTING.md)).

## Consequences

- LICENSE replaced with Apache-2.0 and a NOTICE file added (done 2026-10-22).
- Contributions are accepted under Apache-2.0 terms (section 5 of the licence), with DCO sign-off.
- Code from GPL-licensed projects (e.g. Frappe Lending) must not be copied into Tamarind; learning from their designs is fine.
- If protection against closed hosted forks becomes important later, it can be addressed with trademarks and governance, or by a new ADR.
