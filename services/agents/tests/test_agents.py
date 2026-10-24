"""Unit tests: agent playbooks against a fake gateway (no server needed)."""

import tempfile
import unittest
from pathlib import Path

from tamarind_agents.agents import UnderwritingAgent, VerificationAgent
from tamarind_agents.client import ToolResult
from tamarind_agents.narrative import AnthropicNarrator, TemplateNarrator, narrator_from_env
from tamarind_agents.runtime import Cursor, Worker

APP = "11111111-1111-7111-8111-111111111111"
BIZ = "22222222-2222-7222-8222-222222222222"
DIR = "33333333-3333-7333-8333-333333333333"


def event(to: str, seq: int = 7) -> dict:
    return {"seq": seq, "event_id": f"evt-{seq}", "type": "ApplicationStateChanged", "aggregate_id": APP,
            "data": {"from": "intake", "to": to}}


def application(state: str, **extra) -> dict:
    app = {
        "application_id": APP, "reference": "APP-1", "state": state, "currency": "USD",
        "requested_amount_minor": 5000000, "requested_term_months": 12, "purpose": "stock",
        "parties": [{"party_id": BIZ, "role": "applicant", "kind": "business"},
                    {"party_id": DIR, "role": "director", "kind": "individual"}],
        "consents": [{"party_id": BIZ, "scope": "kyc"}, {"party_id": DIR, "scope": "kyc"}],
        "checks": [],
        "metrics": {"dscr": 1.53, "annual_revenue_minor": 50000000, "bank_months": 12, "negative_balance_days_6m": 1,
                    "estimated_new_payment_minor": 460783},
        "checks_summary": {"all_mandatory_complete": True, "sanctions": {"confirmed_match": False},
                           "kyc": {"all_passed": True}, "kyb": {"status": "passed"}, "bureau": {"serious_default_24m": False}},
    }
    app.update(extra)
    return app


class FakeClient:
    """Records calls and answers from canned responses."""

    def __init__(self, app: dict, policy: dict | None = None, recommend_status: str = "approval_required") -> None:
        self.app, self.policy, self.recommend_status = app, policy, recommend_status
        self.calls: list[tuple[str, dict, str, str | None]] = []
        self.feed: list[dict] = []

    def call(self, tool, args, *, reason, key=None, on_behalf_of=None):
        self.calls.append((tool, args, reason, key))
        if tool == "application.get":
            return ToolResult("ok", {"application": self.app})
        if tool == "policy.evaluate":
            return ToolResult("ok", self.policy)
        if tool == "decision.recommend":
            return ToolResult(self.recommend_status, {}, approval_request_id="apr-1")
        return ToolResult("ok", {"status": "passed"})

    def events(self, after, *, types=None, wait=0, limit=100):
        page = [e for e in self.feed if e["seq"] > after]
        return {"events": page, "next_after": page[-1]["seq"] if page else after}


PASS = {"pack": "baseline/sme-working-capital", "pack_version": "0.1.0", "outcome": "pass",
        "results": [{"rule_id": "ELG-01", "passed": True}], "failed": []}


class VerificationTests(unittest.TestCase):
    def test_wakes_only_on_entering_verification(self):
        a = VerificationAgent()
        self.assertTrue(a.wants(event("verification")))
        self.assertFalse(a.wants(event("underwriting")))
        self.assertFalse(a.wants({"type": "PartyAdded", "data": {}}))

    def test_runs_every_mandatory_check_once(self):
        c = FakeClient(application("verification"))
        out = VerificationAgent().handle(event("verification"), c)
        tools = [(t, a["party_id"]) for t, a, _, _ in c.calls if t != "application.get"]
        self.assertEqual(tools, [("kyc.run", BIZ), ("sanctions.screen", BIZ), ("credit.pull_report", BIZ),
                                 ("bank.analyse_statements", BIZ), ("kyc.run", DIR), ("sanctions.screen", DIR)])
        self.assertEqual(out.action, "acted")
        keys = [k for t, _, _, k in c.calls if t != "application.get"]
        self.assertEqual(len(set(keys)), len(keys), "idempotency keys must be unique per call")
        self.assertTrue(all(k.startswith("verification:evt-7:") for k in keys))
        self.assertTrue(all(r for _, _, r, _ in c.calls), "every call states a reason")

    def test_skips_checks_already_done_and_adds_optional_consumer_report(self):
        app = application("verification", checks=[{"check_type": "kyb", "party_id": BIZ}],
                          consents=[{"party_id": DIR, "scope": "credit_bureau"}])
        steps = VerificationAgent.plan(app)
        self.assertNotIn(("kyc.run", {"party_id": BIZ}, "kyb"), steps)
        self.assertIn(("credit.pull_report", {"party_id": DIR, "report_type": "consumer"}, "credit_bureau"), steps)

    def test_does_nothing_when_state_moved_on(self):
        c = FakeClient(application("underwriting"))
        out = VerificationAgent().handle(event("verification"), c)
        self.assertEqual(out.action, "skipped")
        self.assertEqual([t for t, *_ in c.calls], ["application.get"])


class UnderwritingTests(unittest.TestCase):
    def test_proposes_approval_when_policy_passes(self):
        c = FakeClient(application("underwriting"), PASS)
        out = UnderwritingAgent().handle(event("underwriting"), c)
        tool, args, reason, key = c.calls[-1]
        self.assertEqual(tool, "decision.recommend")
        self.assertEqual(args["outcome"], "approve")
        self.assertEqual(args["reason_codes"], ["ALL_POLICY_RULES_PASSED"])
        self.assertEqual(args["models"][0]["kind"], "rules")
        self.assertIn("DSCR 1.53", args["narrative"])
        self.assertNotIn("Mekong", args["narrative"])
        self.assertEqual(key, "underwriting:evt-7:decision.recommend")
        self.assertIn("approval apr-1", out.detail)

    def test_decline_uses_knock_out_reason_codes_only(self):
        policy = {**PASS, "outcome": "decline", "failed": [
            {"rule_id": "CRD-01", "outcome": "decline", "reason_code": "RECENT_SERIOUS_DEFAULT"},
            {"rule_id": "CRD-02", "outcome": "refer", "reason_code": "LOW_DEBT_SERVICE_COVERAGE"}]}
        c = FakeClient(application("underwriting"), policy)
        UnderwritingAgent().handle(event("underwriting"), c)
        args = c.calls[-1][1]
        self.assertEqual(args["outcome"], "decline")
        self.assertEqual(args["reason_codes"], ["RECENT_SERIOUS_DEFAULT"])

    def test_refer_lists_failed_rules(self):
        policy = {**PASS, "outcome": "refer", "failed": [
            {"rule_id": "CRD-05", "outcome": "refer", "reason_code": "FREQUENT_OVERDRAFT"}]}
        c = FakeClient(application("underwriting"), policy)
        UnderwritingAgent().handle(event("underwriting"), c)
        args = c.calls[-1][1]
        self.assertEqual((args["outcome"], args["reason_codes"]), ("refer", ["FREQUENT_OVERDRAFT"]))
        self.assertIn("CRD-05", args["narrative"])

    def test_skips_when_already_recommended(self):
        c = FakeClient(application("underwriting", recommendation={"outcome": "approve"}), PASS)
        self.assertEqual(UnderwritingAgent().handle(event("underwriting"), c).action, "skipped")


class NarrativeTests(unittest.TestCase):
    def test_llm_failure_falls_back_to_template(self):
        n = AnthropicNarrator("invalid", "test-model")
        n.endpoint = "http://127.0.0.1:9/unreachable"
        facts = UnderwritingAgent.facts(application("underwriting"), PASS)
        text, model = n.write(facts, "approve")
        self.assertIsNone(model)
        self.assertEqual(text, TemplateNarrator().write(facts, "approve")[0])

    def test_env_without_model_uses_template(self):
        import os
        from unittest import mock
        with mock.patch.dict(os.environ, {"TAMARIND_LLM_PROVIDER": "anthropic", "ANTHROPIC_API_KEY": "x"}, clear=False):
            os.environ.pop("TAMARIND_LLM_MODEL", None)
            self.assertIsInstance(narrator_from_env(), TemplateNarrator)


class RuntimeTests(unittest.TestCase):
    def test_cursor_persists_and_events_are_processed_once(self):
        with tempfile.TemporaryDirectory() as d:
            c = FakeClient(application("verification"))
            c.feed = [event("verification", 3), {"seq": 4, "event_id": "e4", "type": "ApplicationStateChanged",
                                                  "aggregate_id": APP, "data": {"to": "intake"}}]
            w = Worker(VerificationAgent(), c, Path(d))
            w.drain()
            self.assertEqual(Cursor(Path(d) / "verification.json").after, 4)
            self.assertEqual(len(w.outcomes), 1)
            n = len(c.calls)
            Worker(VerificationAgent(), c, Path(d)).drain()  # restart: nothing new
            self.assertEqual(len(c.calls), n)


if __name__ == "__main__":
    unittest.main()
