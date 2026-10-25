// Authorization policies for the SME working-capital baseline pack (Cedar, ADR-0004).
//
// Request shape used by the Tool Gateway:
//   principal: Tamarind::User | Tamarind::Agent | Tamarind::Service | Tamarind::Borrower
//   action:    Tamarind::Action::"<tool name>", grouped under action groups below
//   resource:  Tamarind::Application
//
// Delegation: when an agent acts on behalf of a user, the gateway evaluates the request
// twice (once for the agent, once for the user) and allows it only if BOTH are permitted.
//
// Action groups (supplied as action entity parents):
//   "read"      application.get, application.next_actions, policy.evaluate, approval.list,
//               task.list, agent.run_list
//   "intake"    application.create/update/submit, party.add/update, document.*, task.create/complete/remind
//   "verify"    kyc.run, sanctions.screen, credit.pull_report, bank.analyse_statements, evidence.verify
//   "recommend" decision.recommend, decision.refer
//   "offer"     offer.generate, offer.send
//
// Forbid policies always override permit policies.

// --- Guardrails -------------------------------------------------------------

// Tenant isolation: nobody acts on another tenant's applications.
forbid (principal, action, resource)
unless { principal.tenant == resource.tenant };

// Maker-checker: whoever recommended a decision (or was represented by the
// recommending agent) cannot approve or decline it.
forbid (
  principal,
  action in [Tamarind::Action::"decision.approve", Tamarind::Action::"decision.decline"],
  resource
)
when {
  (resource has recommended_by && resource.recommended_by == principal) ||
  (resource has recommended_for && resource.recommended_for == principal)
};

// Agents never make final credit decisions or book loans in the baseline pack.
// Enabling L4 autonomy for a narrow segment requires a reviewed change to this policy.
forbid (
  principal is Tamarind::Agent,
  action in [
    Tamarind::Action::"decision.approve",
    Tamarind::Action::"decision.decline",
    Tamarind::Action::"loan.handoff",
    Tamarind::Action::"approval.respond"
  ],
  resource
);

// --- Staff --------------------------------------------------------------------

permit (
  principal in Tamarind::Role::"loan_officer",
  action in [
    Tamarind::Action::"read",
    Tamarind::Action::"intake",
    Tamarind::Action::"verify",
    Tamarind::Action::"recommend",
    Tamarind::Action::"offer"
  ],
  resource
);

// Approvers decide within their approval limit (minor units). The gateway loads each
// approver's approval_limit_minor from pack.yaml approval_authority; senior credit
// approvers are members of the credit_approver role with the higher limit.
permit (
  principal in Tamarind::Role::"credit_approver",
  action == Tamarind::Action::"decision.approve",
  resource
)
when { resource.requested_amount_minor <= principal.approval_limit_minor };

permit (
  principal in Tamarind::Role::"credit_approver",
  action in [Tamarind::Action::"decision.decline", Tamarind::Action::"read"],
  resource
);

permit (
  principal in Tamarind::Role::"operations",
  action in [Tamarind::Action::"read", Tamarind::Action::"loan.handoff"],
  resource
);

// Every member of staff works the shared task queue.
permit (
  principal,
  action in [Tamarind::Action::"task.create", Tamarind::Action::"task.complete"],
  resource
)
when {
  principal in Tamarind::Role::"credit_approver" ||
  principal in Tamarind::Role::"operations"
};

// Staff may respond to approval requests. When a request is approved, the gated tool
// runs as the responder and is authorized again for them, so a responder can only
// approve actions they are themselves allowed to take.
permit (
  principal,
  action == Tamarind::Action::"approval.respond",
  resource
)
when {
  principal in Tamarind::Role::"loan_officer" ||
  principal in Tamarind::Role::"credit_approver" ||
  principal in Tamarind::Role::"operations"
};

// --- Agents -------------------------------------------------------------------

// Every agent records its runs and escalates to people by creating tasks. Which
// application it may touch is still limited by the tenant guardrail above.
permit (
  principal is Tamarind::Agent,
  action in [
    Tamarind::Action::"agent.run_start",
    Tamarind::Action::"agent.run_finish",
    Tamarind::Action::"task.create"
  ],
  resource
);

permit (
  principal in Tamarind::Role::"agent_intake",
  action in [Tamarind::Action::"read", Tamarind::Action::"intake"],
  resource
);

permit (
  principal in Tamarind::Role::"agent_verification",
  action in [Tamarind::Action::"read", Tamarind::Action::"verify"],
  resource
);

// The document agent classifies and extracts documents and verifies evidence (above
// the confidence threshold the gateway enforces). It cannot upload, attach or request.
permit (
  principal in Tamarind::Role::"agent_documents",
  action in [
    Tamarind::Action::"read",
    Tamarind::Action::"document.classify",
    Tamarind::Action::"document.extract",
    Tamarind::Action::"evidence.verify"
  ],
  resource
);

permit (
  principal in Tamarind::Role::"agent_underwriting",
  action in [Tamarind::Action::"read", Tamarind::Action::"recommend"],
  resource
);

// --- Borrowers ----------------------------------------------------------------

// Borrowers act only on their own application, through a narrow set of tools.
permit (
  principal is Tamarind::Borrower,
  action in [
    Tamarind::Action::"application.get",
    Tamarind::Action::"document.upload_url",
    Tamarind::Action::"document.attach",
    Tamarind::Action::"consent.record",
    Tamarind::Action::"offer.accept"
  ],
  resource
)
when { resource has borrower && resource.borrower == principal };
