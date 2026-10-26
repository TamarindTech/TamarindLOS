"""Unit tests: agent playbooks against a fake gateway (no server needed)."""

import tempfile
import unittest
import unittest.mock
from pathlib import Path

from tamarind_agents.agents import DocumentAgent, IntakeAgent, UnderwritingAgent, VerificationAgent
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
        self.run_id: str | None = None
        self.runs: list[tuple[str, str | None]] = []  # (tool, run id in the envelope)

    def call(self, tool, args, *, reason, key=None, on_behalf_of=None):
        self.calls.append((tool, args, reason, key))
        self.runs.append((tool, self.run_id))
        if tool == "agent.run_start":
            return ToolResult("ok", {"run_id": "run-1"})
        if tool == "agent.run_finish":
            return ToolResult("ok", {"run_id": args["run_id"], "status": args["status"]})
        if tool == "task.create":
            return ToolResult("ok", {"task_id": "task-1"})
        if tool == "document.request":
            return ToolResult("ok", {"task_id": "req-" + args["document_type"], "notified_via": "portal"})
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
        "results": [{"rule_id": "ELG-01", "passed": True}], "failed": [],
        "risk_grade": {"grade": "A", "points": 75, "max_points": 100, "apr_bps": 1400, "missing": []}}


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


class DocClient(FakeClient):
    def __init__(self, app, doc_type="business_registration", confidence=0.97, evidence=None, low=()):
        super().__init__(app)
        self.doc_type, self.confidence, self.low = doc_type, confidence, list(low)
        self.evidence = evidence or []

    def call(self, tool, args, *, reason, key=None, on_behalf_of=None):
        if tool == "document.classify":
            self.calls.append((tool, args, reason, key))
            self.runs.append((tool, self.run_id))
            return ToolResult("ok", {"document_type": self.doc_type, "confidence": self.confidence})
        if tool == "document.extract":
            self.calls.append((tool, args, reason, key))
            self.runs.append((tool, self.run_id))
            return ToolResult("ok", {"evidence": self.evidence, "low_confidence_fields": self.low})
        return super().call(tool, args, reason=reason, key=key)


def attached(party=BIZ):
    return {"seq": 9, "event_id": "evt-9", "type": "DocumentAttached", "aggregate_id": APP,
            "data": {"document_id": "doc-1", "party_id": party}}


class DocumentTests(unittest.TestCase):
    def app(self):
        app = application("intake")
        app["parties"][0].update(legal_name="Mekong Trading Co.", registration_number="0105561234567")
        app["parties"][1].update(legal_name="Somchai P.", date_of_birth="1980-04-02")
        return app

    def verdicts(self, c):
        return {a["evidence_id"]: a["status"] for t, a, _, _ in c.calls if t == "evidence.verify"}

    def test_verifies_matching_confident_fields_and_flags_the_rest(self):
        c = DocClient(self.app(), evidence=[
            {"evidence_id": "e1", "field": "registration_number", "value": "0105561234567", "confidence": 0.95},
            {"evidence_id": "e2", "field": "legal_name", "value": "MEKONG TRADING CO", "confidence": 0.95},
            {"evidence_id": "e3", "field": "legal_name", "value": "Other Company Ltd", "confidence": 0.95},
            {"evidence_id": "e4", "field": "address", "value": "88 Riverside", "confidence": 0.6}], low=["address"])
        out = DocumentAgent().handle(attached(), c)
        self.assertEqual(self.verdicts(c), {"e1": "verified", "e2": "verified", "e3": "needs_review", "e4": "needs_review"})
        self.assertIn("2 for a person", out.detail)
        # The fields a person must look at become one review task.
        self.assertEqual(out.action, "escalated")
        self.assertEqual(out.task["kind"], "review_evidence")
        self.assertEqual([e["evidence_id"] for e in out.task["details"]["evidence"]], ["e3", "e4"])

    def test_identity_document_checked_against_the_director(self):
        c = DocClient(self.app(), doc_type="identity_document", evidence=[
            {"evidence_id": "e1", "field": "date_of_birth", "value": "1980-04-02", "confidence": 0.95},
            {"evidence_id": "e2", "field": "legal_name", "value": "Somchai P.", "confidence": 0.95}])
        DocumentAgent().handle(attached(DIR), c)
        self.assertEqual(set(self.verdicts(c).values()), {"verified"})

    def test_unrecognised_document_goes_to_a_person(self):
        c = DocClient(self.app(), doc_type="other", confidence=0.3)
        out = DocumentAgent().handle(attached(), c)
        self.assertEqual(out.action, "escalated")
        self.assertEqual(out.task["kind"], "review_document")
        self.assertEqual([t for t, *_ in c.calls], ["document.classify"])

    def test_all_fields_verified_is_not_an_escalation(self):
        c = DocClient(self.app(), evidence=[
            {"evidence_id": "e1", "field": "registration_number", "value": "0105561234567", "confidence": 0.95}])
        out = DocumentAgent().handle(attached(), c)
        self.assertEqual((out.action, out.task), ("acted", None))


def review() -> dict:
    return {"seq": 5, "event_id": "evt-5", "type": "IntakeReviewDue", "aggregate_id": APP, "data": {}}


def overdue(left: int, reminders: int = 0) -> dict:
    return {"seq": 6, "event_id": "evt-6", "type": "TaskOverdue", "aggregate_id": APP,
            "data": {"task_id": "req-1", "kind": "document_request", "reminders": reminders, "reminders_left": left,
                     "details": {"document_type": "bank_statements", "party_id": BIZ}}}


class IntakeTests(unittest.TestCase):
    def test_requests_each_missing_document_once(self):
        app = application("intake", missing_document_items=[
            {"document_type": "bank_statements", "party_id": BIZ, "party_role": "applicant"},
            {"document_type": "identity_document", "party_id": DIR, "party_role": "director"}])
        c = FakeClient(app)
        out = IntakeAgent().handle(review(), c)
        reqs = [(a["party_id"], a["document_type"], k) for t, a, _, k in c.calls if t == "document.request"]
        self.assertEqual(reqs, [(BIZ, "bank_statements", f"intake:evt-5:request:{BIZ}:bank_statements"),
                                (DIR, "identity_document", f"intake:evt-5:request:{DIR}:identity_document")])
        self.assertEqual(out.detail, "requested 2 document(s)")

    def test_uploaded_but_unclassified_documents_are_not_requested(self):
        app = application("intake", missing_document_items=[
            {"document_type": "bank_statements", "party_id": BIZ, "party_role": "applicant"}],
            documents=[{"document_id": "d1", "party_id": BIZ, "declared_type": "bank_statements", "status": "received"}])
        out = IntakeAgent().handle(review(), FakeClient(app))
        self.assertEqual(out.action, "skipped")

    def test_nothing_missing_or_moved_on(self):
        self.assertEqual(IntakeAgent().handle(review(), FakeClient(application("intake"))).action, "skipped")
        self.assertEqual(IntakeAgent().handle(review(), FakeClient(application("verification"))).action, "skipped")

    def test_waits_for_the_grace_period_not_submission(self):
        a = IntakeAgent()
        self.assertTrue(a.wants(review()))
        self.assertFalse(a.wants(event("intake")))
        self.assertTrue(a.wants(overdue(1)))
        self.assertFalse(a.wants({**overdue(1), "data": {"kind": "review_evidence"}}))

    def test_overdue_request_is_reminded(self):
        c = FakeClient(application("intake"))
        out = IntakeAgent().handle(overdue(1), c)
        tool, args, _, key = c.calls[-1]
        self.assertEqual((tool, args["task_id"], key), ("task.remind", "req-1", "intake:evt-6:remind"))
        self.assertIn("bank statements", args["message"])
        self.assertEqual(out.action, "acted")

    def test_a_closed_request_is_not_chased(self):
        c = FakeClient(application("intake"))
        orig = c.call

        def call(tool, args, **kw):
            if tool == "task.remind":
                return ToolResult("error", error_code="STATE_CONFLICT", error_message="task is already done",
                                  error_details={"status": "done"})
            return orig(tool, args, **kw)
        c.call = call
        out = IntakeAgent().handle(overdue(1), c)
        self.assertEqual((out.action, out.task), ("skipped", None))

    def test_after_the_last_reminder_a_person_takes_over(self):
        c = FakeClient(application("intake"))
        out = IntakeAgent().handle(overdue(0, reminders=2), c)
        self.assertEqual([t for t, *_ in c.calls], [])
        self.assertEqual(out.action, "escalated")
        self.assertEqual(out.task["kind"], "collect_document")
        self.assertIn("after 2 reminder(s)", out.task["title"])
        self.assertEqual(out.task["details"]["document_request_task_id"], "req-1")


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
        self.assertIn("Risk grade: A (75 of 100 points), priced at 14.00% APR.", args["narrative"])
        self.assertNotIn("terms", args)  # the gateway prices at the grade
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
    def worker(self, app, d):
        c = FakeClient(app)
        return c, Worker(VerificationAgent(), c, Path(d))

    def test_each_event_is_one_run_and_every_call_carries_it(self):
        with tempfile.TemporaryDirectory() as d:
            c, w = self.worker(application("verification"), d)
            w.process(event("verification"))
            start, *work, finish = c.runs
            self.assertEqual(start, ("agent.run_start", None))
            self.assertTrue(work and all(run == "run-1" for _, run in work))
            self.assertEqual(finish, ("agent.run_finish", None))
            args = c.calls[-1][1]
            self.assertEqual((args["run_id"], args["status"]), ("run-1", "succeeded"))
            start_args = c.calls[0][1]
            self.assertEqual(start_args["trigger_event_id"], "evt-7")
            self.assertEqual(start_args["application_id"], APP)
            self.assertIsNone(c.run_id)

    def test_escalation_creates_a_task_and_the_run_records_it(self):
        with tempfile.TemporaryDirectory() as d:
            c = FakeClient(application("verification"))
            # A KYC check that needs review.
            orig = c.call

            def call(tool, args, **kw):
                r = orig(tool, args, **kw)
                if tool == "kyc.run" and args["party_id"] == DIR:
                    return ToolResult("ok", {"status": "needs_review"})
                return r
            c.call = call
            Worker(VerificationAgent(), c, Path(d)).process(event("verification"))
            task = next(a for t, a, *_ in c.calls if t == "task.create")
            self.assertEqual((task["kind"], task["assignee"], task["application_id"]), ("review_check", "role:loan_officer", APP))
            self.assertEqual(task["details"]["agent"], "agent:verification")
            finish = c.calls[-1][1]
            self.assertEqual((finish["status"], finish["escalation_task_id"]), ("escalated", "task-1"))

    def test_gateway_outage_leaves_the_run_open_for_the_retry(self):
        from tamarind_agents import runtime
        from tamarind_agents.client import GatewayError
        with tempfile.TemporaryDirectory() as d:
            c = FakeClient(application("verification"))
            orig = c.call

            def call(tool, args, **kw):
                if tool == "kyc.run":
                    raise GatewayError("connection refused")
                return orig(tool, args, **kw)
            c.call = call
            w = Worker(VerificationAgent(), c, Path(d))
            with unittest.mock.patch.object(runtime.time, "sleep"):
                with self.assertRaises(GatewayError):
                    w.process(event("verification"))
            self.assertNotIn("agent.run_finish", [t for t, *_ in c.calls])
            self.assertIsNone(c.run_id)

    def test_a_run_left_open_is_superseded(self):
        with tempfile.TemporaryDirectory() as d:
            c = FakeClient(application("verification"))
            orig, starts = c.call, []

            def call(tool, args, **kw):
                if tool == "agent.run_start":
                    starts.append(1)
                    if len(starts) == 1:
                        return ToolResult("error", error_code="STATE_CONFLICT", error_message="open run",
                                          error_details={"run_id": "run-old"})
                return orig(tool, args, **kw)
            c.call = call
            Worker(VerificationAgent(), c, Path(d)).process(event("verification"))
            finishes = [(a["run_id"], a["status"]) for t, a, *_ in c.calls if t == "agent.run_finish"]
            self.assertEqual(finishes, [("run-old", "failed"), ("run-1", "succeeded")])
            self.assertTrue(all(run == "run-1" for t, run in c.runs if t == "kyc.run"))

    def test_a_reprocessed_event_does_not_reattach_the_run(self):
        with tempfile.TemporaryDirectory() as d:
            c = FakeClient(application("verification"))
            orig = c.call

            def call(tool, args, **kw):
                r = orig(tool, args, **kw)
                if tool == "agent.run_start":
                    r.replayed = True
                return r
            c.call = call
            Worker(VerificationAgent(), c, Path(d)).process(event("verification"))
            self.assertTrue(all(run is None for t, run in c.runs if t == "kyc.run"))
            self.assertIn("agent.run_finish", [t for t, *_ in c.calls])

    def test_closing_a_run_is_retried(self):
        from tamarind_agents import runtime
        from tamarind_agents.client import GatewayError
        with tempfile.TemporaryDirectory() as d:
            c = FakeClient(application("verification"))
            orig, failures = c.call, []

            def call(tool, args, **kw):
                if tool == "agent.run_finish" and not failures:
                    failures.append(1)
                    raise GatewayError("connection reset")
                return orig(tool, args, **kw)
            c.call = call
            with unittest.mock.patch.object(runtime.time, "sleep"):
                Worker(VerificationAgent(), c, Path(d)).process(event("verification"))
            self.assertEqual([a["status"] for t, a, *_ in c.calls if t == "agent.run_finish"], ["succeeded"])

    def test_skipped_event_closes_the_run_as_cancelled(self):
        with tempfile.TemporaryDirectory() as d:
            c, w = self.worker(application("underwriting"), d)
            w.process(event("verification"))
            self.assertEqual(c.calls[-1][1]["status"], "cancelled")

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
