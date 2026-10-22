# Contributing to Tamarind LOS

Thank you for your interest in Tamarind. The project is in its **design phase**, so the most valuable contributions right now are ideas, reviews and domain expertise, especially from people who work in lending, credit risk, compliance or AI engineering.

## Ways to contribute

- **Review the design.** Read the [concept paper](docs/concept/01-agent-native-concept-paper.md) and [reference architecture](docs/architecture/02-reference-architecture.md), then open an issue with questions, gaps or disagreements.
- **Weigh in on decisions.** Comment on any ADR with status *Proposed* in [docs/adr](docs/adr/README.md).
- **Share domain knowledge.** Lending products, document types, regulatory requirements and data providers in your market all help shape policy packs and connectors.
- **Code.** Implementation starts in Phase 0. Look for issues labelled `good first issue` once they appear.

## Proposing an architecture decision

1. Copy [docs/adr/0000-template.md](docs/adr/0000-template.md) to the next free number, e.g. `docs/adr/0005-short-title.md`.
2. Fill in context, options and a recommendation. Set the status to **Proposed**.
3. Open a pull request. Discussion happens on the PR.
4. When maintainers accept it, the status changes to **Accepted**. Later changes are made by a new ADR that **supersedes** the old one, rather than by rewriting history.

## Pull requests

- Keep each PR focused on one change.
- Use [Conventional Commits](https://www.conventionalcommits.org) for commit messages, e.g. `docs: add ADR for workflow engine` or `feat(gateway): validate tool input schemas`.
- Sign off your commits to certify the [Developer Certificate of Origin](https://developercertificate.org): `git commit -s`.
- Update documentation when behaviour or interfaces change.
- Once code exists: add tests, and make sure CI passes.

## Documentation style

- Write in plain English. Define acronyms on first use.
- Prefer tables and diagrams (Mermaid) where they make structure clearer.
- Keep documents jurisdiction-neutral unless they are explicitly about a specific market.

## Reporting security issues

Please **do not** open public issues for security vulnerabilities. Use GitHub's private vulnerability reporting (the **Security** tab of this repository) so the issue can be fixed before disclosure.

## Code of Conduct

By participating, you agree to follow the [Code of Conduct](CODE_OF_CONDUCT.md).
