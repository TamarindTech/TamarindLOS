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
        states = [c for c in out.calls if c["check"] in ("needs_review", "failed")]
        out.detail = f"{len(out.calls)} checks run" + (f", {len(states)} need review" if states else "")
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
    return {"verification": VerificationAgent(), "underwriting": UnderwritingAgent(narrator)}
