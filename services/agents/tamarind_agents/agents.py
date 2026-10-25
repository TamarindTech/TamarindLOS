"""Tamarind's built-in agents (Reference Architecture §9.1).

Each agent wakes on a domain event, reads the application snapshot and calls
tools through the gateway. Agents hold no privileges of their own: the gateway
decides, per call, whether the agent may act alone (L3), must ask a person
(L2), or may not act at all. The agents here are deliberately deterministic
playbooks; an LLM is used only to draft the credit memo narrative.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass, field
from typing import Any, Protocol

from .client import ToolResult
from .narrative import Narrator, TemplateNarrator

log = logging.getLogger("tamarind.agents")


class Client(Protocol):
    def call(self, tool: str, args: dict[str, Any], *, reason: str, key: str | None = None,
             on_behalf_of: str | None = None) -> ToolResult: ...


@dataclass
class Outcome:
    """What an agent did with one event (logged, and returned for tests)."""

    action: str  # acted | skipped | escalated
    detail: str = ""
    calls: list[dict[str, Any]] = field(default_factory=list)
    # For escalations: the task to hand to a person (kind, title, details, assignee).
    # The runtime creates it; without one it files a generic resolve_exception task.
    task: dict[str, Any] | None = None


class Agent:
    name = "agent"
    principal = "agent:unknown"
    event_types: tuple[str, ...] = ("ApplicationStateChanged",)

    def wants(self, event: dict[str, Any]) -> bool:
        raise NotImplementedError

    def handle(self, event: dict[str, Any], client: Client) -> Outcome:
        raise NotImplementedError

    # --- helpers ---------------------------------------------------------------

    def key(self, event: dict[str, Any], *parts: str) -> str:
        """Idempotency key tied to the triggering event: reprocessing replays, never repeats."""
        return ":".join([self.name, event["event_id"], *parts])

    @staticmethod
    def entered(event: dict[str, Any], state: str) -> bool:
        return event.get("type") == "ApplicationStateChanged" and (event.get("data") or {}).get("to") == state

    def snapshot(self, client: Client, app_id: str, reason: str) -> dict[str, Any] | None:
        r = client.call("application.get", {"application_id": app_id}, reason=reason)
        if not r.ok:
            log.warning("%s: cannot read application %s: %s %s", self.name, app_id, r.error_code, r.error_message)
            return None
        return r.result.get("application")


# --- Verification agent ---------------------------------------------------------------


class VerificationAgent(Agent):
    """Runs identity, sanctions, bureau and bank-data checks once intake is complete."""

    name = "verification"
    principal = "agent:verification"

    def wants(self, event: dict[str, Any]) -> bool:
        return self.entered(event, "verification")

    @staticmethod
    def plan(app: dict[str, Any]) -> list[tuple[str, dict[str, Any], str]]:
        """Checks to run as (tool, extra input, check type), skipping those already done."""
        done = {(c["check_type"], c["party_id"]) for c in app.get("checks", [])}
        consents = {(c["party_id"], c["scope"]) for c in app.get("consents", [])}
        steps: list[tuple[str, dict[str, Any], str]] = []
        for p in app.get("parties", []):
            pid, role = p["party_id"], p["role"]
            wanted: list[tuple[str, dict[str, Any], str]]
            if p["kind"] == "business":
                wanted = [("kyc.run", {}, "kyb"), ("sanctions.screen", {}, "sanctions")]
                if role == "applicant":
                    wanted += [("credit.pull_report", {"report_type": "commercial"}, "credit_bureau"),
                               ("bank.analyse_statements", {"source": "open_banking"}, "bank_analysis")]
            else:
                wanted = [("kyc.run", {}, "kyc"), ("sanctions.screen", {}, "sanctions")]
                # Consumer reports on individuals are optional: only with their consent.
                if (pid, "credit_bureau") in consents:
                    wanted.append(("credit.pull_report", {"report_type": "consumer"}, "credit_bureau"))
            for tool, extra, check_type in wanted:
                if (check_type, pid) not in done:
                    steps.append((tool, {"party_id": pid, **extra}, check_type))
        return steps

    def handle(self, event: dict[str, Any], client: Client) -> Outcome:
        app_id = event["aggregate_id"]
        app = self.snapshot(client, app_id, "verification: read application before running checks")
        if app is None:
            return Outcome("escalated", "application not readable")
        if app["state"] != "verification":
            return Outcome("skipped", f"application is now {app['state']}")
        out = Outcome("acted")
        for tool, args, check_type in self.plan(app):
            r = client.call(tool, {"application_id": app_id, **args},
                            reason=f"verification: mandatory {check_type} check after intake completed",
                            key=self.key(event, tool, args["party_id"]))
            call = {"tool": tool, "party_id": args["party_id"], "status": r.status,
                    "check": r.result.get("status") or r.error_code}
            out.calls.append(call)
            if not r.ok:
                log.warning("verification: %s on %s failed: %s %s", tool, args["party_id"], r.error_code, r.error_message)
        flagged = [c for c in out.calls if c["check"] in ("needs_review", "failed")]
        out.detail = f"{len(out.calls)} checks run" + (f", {len(flagged)} need review" if flagged else "")
        if flagged:
            out.action = "escalated"
            out.task = {"kind": "review_check", "title": f"Review {len(flagged)} verification result(s) on {app.get('reference', app_id)}",
                        "details": {"checks": flagged}}
        return out


# --- Document agent ------------------------------------------------------------------


def _norm(v: Any) -> str:
    return "".join(ch for ch in str(v).lower() if ch.isalnum())


class DocumentAgent(Agent):
    """Classifies and extracts each uploaded document, then checks the evidence.

    Evidence is verified only when it was read confidently and agrees with what the
    application already says; anything else goes to a person as needs_review. The
    gateway enforces the confidence threshold too, so the agent cannot overreach.
    """

    name = "documents"
    principal = "agent:documents"
    event_types = ("DocumentAttached",)
    min_classification_confidence = 0.5

    def wants(self, event: dict[str, Any]) -> bool:
        return event.get("type") == "DocumentAttached"

    @staticmethod
    def references(app: dict[str, Any], party_id: str) -> dict[str, str]:
        """Values the application already holds, to compare extracted fields against."""
        parties = app.get("parties", [])
        party = next((p for p in parties if p["party_id"] == party_id), None)
        applicant = next((p for p in parties if p["role"] == "applicant"), None)
        party = party or applicant or {}
        refs = {"legal_name": party.get("legal_name", ""), "registration_number": party.get("registration_number", ""),
                "date_of_birth": party.get("date_of_birth", "")}
        if applicant:
            refs["account_holder"] = applicant.get("legal_name", "")
        return {k: v for k, v in refs.items() if v}

    def handle(self, event: dict[str, Any], client: Client) -> Outcome:
        data = event.get("data") or {}
        doc_id, app_id = data.get("document_id"), event["aggregate_id"]
        out = Outcome("acted")
        cl = client.call("document.classify", {"document_id": doc_id}, reason="documents: classify a new upload",
                         key=self.key(event, "classify"))
        out.calls.append({"tool": "document.classify", "status": cl.status, "type": cl.result.get("document_type"),
                          "error": cl.error_code})
        if not cl.ok:
            return Outcome("escalated", f"classification failed: {cl.error_code}", out.calls)
        doc_type, confidence = cl.result.get("document_type"), cl.result.get("confidence", 0)
        if doc_type == "other" or confidence < self.min_classification_confidence:
            return Outcome("escalated", "document not recognised; a person must look at it", out.calls,
                           task={"kind": "review_document", "title": "Identify an unrecognised document",
                                 "details": {"document_id": doc_id, "classified_as": doc_type, "confidence": confidence}})

        ex = client.call("document.extract", {"document_id": doc_id}, reason=f"documents: extract fields from {doc_type}",
                         key=self.key(event, "extract"))
        out.calls.append({"tool": "document.extract", "status": ex.status, "error": ex.error_code})
        if not ex.ok:
            return Outcome("escalated", f"extraction failed: {ex.error_code}", out.calls)
        low = set(ex.result.get("low_confidence_fields") or [])
        app = self.snapshot(client, app_id, "documents: compare extracted fields with the application") or {}
        refs = self.references(app, data.get("party_id") or "")

        reviews: list[dict[str, Any]] = []
        for e in ex.result.get("evidence") or []:
            field = e["field"]
            if field in low:
                status, note = "needs_review", "low extraction confidence"
            elif field in refs and _norm(e["value"]) != _norm(refs[field]):
                status, note = "needs_review", f"{field} does not match the application record"
            else:
                status, note = "verified", "matches the application" if field in refs else "read with high confidence"
            r = client.call("evidence.verify", {"evidence_id": e["evidence_id"], "status": status, "note": note},
                            reason=f"documents: check {field}", key=self.key(event, "verify", e["evidence_id"]))
            out.calls.append({"tool": "evidence.verify", "field": field, "status": r.status, "result": status,
                              "error": r.error_code})
            if status == "needs_review":
                reviews.append({"evidence_id": e["evidence_id"], "field": field, "note": note})
        count = len(ex.result.get("evidence") or [])
        out.detail = f"{doc_type} ({confidence:.2f}): {count} fields" + (f", {len(reviews)} for a person" if reviews else "")
        if reviews:
            out.action = "escalated"
            out.task = {"kind": "review_evidence",
                        "title": f"Review {', '.join(r['field'] for r in reviews)} on {doc_type.replace('_', ' ')}",
                        "details": {"document_id": doc_id, "evidence": reviews}}
        return out


# --- Intake agent ---------------------------------------------------------------------


class IntakeAgent(Agent):
    """Asks for whatever the application still lacks once it is submitted.

    Requests go to the borrower's portal (or to the loan officers when there is no
    borrower login). The gateway keeps one open request per document and party, and
    the request closes itself when a document of that type is classified.
    """

    name = "intake"
    principal = "agent:intake"

    def wants(self, event: dict[str, Any]) -> bool:
        return self.entered(event, "intake")

    def handle(self, event: dict[str, Any], client: Client) -> Outcome:
        app_id = event["aggregate_id"]
        app = self.snapshot(client, app_id, "intake: read application after submission")
        if app is None:
            return Outcome("escalated", "application not readable")
        if app["state"] != "intake":
            return Outcome("skipped", f"application is now {app['state']}")
        # A document already uploaded for that slot is waiting for classification: don't ask again.
        pending = {(d.get("party_id", ""), d.get("declared_type")) for d in app.get("documents") or []
                   if d.get("status") == "received"}
        missing = [m for m in app.get("missing_document_items") or []
                   if (m["party_id"], m["document_type"]) not in pending and ("", m["document_type"]) not in pending]
        if not missing:
            return Outcome("skipped", "nothing missing")
        out = Outcome("acted")
        for item in missing:
            doc_type, party = item["document_type"], item["party_id"]
            r = client.call("document.request", {"application_id": app_id, "party_id": party, "document_type": doc_type,
                                                 "message": f"Please upload the {doc_type.replace('_', ' ')}."},
                            reason=f"intake: {doc_type} is required and missing", key=self.key(event, "request", party, doc_type))
            out.calls.append({"tool": "document.request", "document_type": doc_type, "party_id": party, "status": r.status,
                              "via": r.result.get("notified_via"), "error": r.error_code})
        failed = [c for c in out.calls if c["status"] != "ok"]
        out.detail = f"requested {len(out.calls) - len(failed)} document(s)"
        if failed:
            return Outcome("escalated", out.detail + f"; {len(failed)} request(s) failed", out.calls)
        return out


# --- Underwriting analyst ----------------------------------------------------------


OUTCOME_FOR_POLICY = {"pass": "approve", "refer": "refer", "decline": "decline"}
CONFIDENCE = {"pass": 0.85, "refer": 0.6, "decline": 0.9}


class UnderwritingAgent(Agent):
    """Evaluates policy and proposes a recommendation for a person to confirm (autonomy L2)."""

    name = "underwriting"
    principal = "agent:underwriting"

    def __init__(self, narrator: Narrator | None = None) -> None:
        self.narrator = narrator or TemplateNarrator()

    def wants(self, event: dict[str, Any]) -> bool:
        return self.entered(event, "underwriting")

    @staticmethod
    def reason_codes(policy: dict[str, Any]) -> list[str]:
        failed = policy.get("failed") or []
        if policy["outcome"] == "pass":
            return ["ALL_POLICY_RULES_PASSED"]
        if policy["outcome"] == "decline":
            failed = [f for f in failed if f.get("outcome") == "decline"]
        codes = [f["reason_code"] for f in failed if f.get("reason_code")]
        return sorted(set(codes)) or ["POLICY_" + policy["outcome"].upper()]

    @staticmethod
    def facts(app: dict[str, Any], policy: dict[str, Any]) -> dict[str, Any]:
        """The memo's inputs: terms, metrics, check summary and rule results. No personal data."""
        results = policy.get("results") or []
        return {
            "reference": app.get("reference"),
            "currency": app.get("currency"),
            "requested_amount_minor": app.get("requested_amount_minor"),
            "requested_term_months": app.get("requested_term_months"),
            "purpose": app.get("purpose"),
            "metrics": app.get("metrics") or {},
            "checks": app.get("checks_summary") or {},
            "documents": {
                "count": len(app.get("documents") or []),
                "evidence_verified": sum(1 for e in app.get("evidence") or [] if e.get("status") == "verified"),
                "evidence_for_review": sum(1 for e in app.get("evidence") or [] if e.get("status") == "needs_review"),
            },
            "policy": {"pack": policy.get("pack"), "version": policy.get("pack_version"), "outcome": policy["outcome"],
                       "rules_passed": sum(1 for r in results if r.get("passed")), "rules_total": len(results),
                       "failed": policy.get("failed") or []},
        }

    def handle(self, event: dict[str, Any], client: Client) -> Outcome:
        app_id = event["aggregate_id"]
        app = self.snapshot(client, app_id, "underwriting: read application for analysis")
        if app is None:
            return Outcome("escalated", "application not readable")
        if app["state"] != "underwriting":
            return Outcome("skipped", f"application is now {app['state']}")
        if app.get("recommendation"):
            return Outcome("skipped", "a recommendation already exists")

        ev = client.call("policy.evaluate", {"application_id": app_id}, reason="underwriting: evaluate the policy pack")
        if not ev.ok:
            return Outcome("escalated", f"policy.evaluate failed: {ev.error_code}")
        policy = ev.result
        outcome = OUTCOME_FOR_POLICY[policy["outcome"]]
        facts = self.facts(app, policy)
        narrative, model = self.narrator.write(facts, outcome)
        models = [{"kind": "rules", "id": policy.get("pack", ""), "version": policy.get("pack_version", "")}]
        if model:
            models.append(model)
        args: dict[str, Any] = {
            "application_id": app_id,
            "outcome": outcome,
            "reason_codes": self.reason_codes(policy),
            "narrative": narrative,
            "confidence": CONFIDENCE[policy["outcome"]],
            "models": models,
        }
        r = client.call("decision.recommend", args,
                        reason=f"underwriting: policy outcome {policy['outcome']}; proposing {outcome}",
                        key=self.key(event, "decision.recommend"))
        call = {"tool": "decision.recommend", "status": r.status, "outcome": outcome,
                "approval_request_id": r.approval_request_id, "error": r.error_code}
        if r.status == "approval_required":
            return Outcome("acted", f"proposed {outcome}; waiting for approval {r.approval_request_id}", [call])
        if r.ok:
            return Outcome("acted", f"recorded {outcome}", [call])
        return Outcome("escalated", f"decision.recommend failed: {r.error_code} {r.error_message}", [call])


def builtin_agents(narrator: Narrator | None = None) -> dict[str, Agent]:
    return {"intake": IntakeAgent(), "documents": DocumentAgent(), "verification": VerificationAgent(),
            "underwriting": UnderwritingAgent(narrator)}
