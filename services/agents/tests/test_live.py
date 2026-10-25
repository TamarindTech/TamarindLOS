"""End-to-end: the agents against a running gateway.

Skipped unless TAMARIND_URL points at a server started with the development
principals, for example:

    TAMARIND_URL=http://localhost:8080 python -m unittest discover -s tests
"""

import json
import os
import tempfile
import unittest
import urllib.request
import uuid
from pathlib import Path

from tamarind_agents.agents import DocumentAgent, UnderwritingAgent, VerificationAgent
from tamarind_agents.client import TamarindClient
from tamarind_agents.runtime import Worker

URL = os.environ.get("TAMARIND_URL", "")


def rest(token: str, tool: str, inp: dict) -> dict:
    body = json.dumps({"input": inp, "idempotency_key": str(uuid.uuid4())}).encode()
    req = urllib.request.Request(f"{URL}/v1/tools/{tool}", data=body, method="POST",
                                 headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req) as r:
            return json.loads(r.read())
    except urllib.error.HTTPError as e:
        return json.loads(e.read())


def put(path: str, content: bytes) -> int:
    req = urllib.request.Request(f"{URL}{path}", data=content, method="PUT")
    try:
        with urllib.request.urlopen(req) as r:
            return r.status
    except urllib.error.HTTPError as e:
        return e.code


def mock_doc(doc_type: str, *fields: str) -> bytes:
    return ("%PDF-1.4\n% TAMARIND-DOC: " + doc_type + "\n" + "".join(f + "\n" for f in fields) + "%%EOF\n").encode()


def upload(app: str, party: str, doc_type: str, content: bytes) -> None:
    """The borrower uploads through the portal: link, PUT, attach. The document agent does the rest."""
    r = rest("dev-borrower-mekong", "document.upload_url", {"application_id": app, "party_id": party,
             "file_name": f"{doc_type}.pdf", "mime_type": "application/pdf"})
    assert put(r["result"]["upload_url"], content) == 201
    r = rest("dev-borrower-mekong", "document.attach", {"application_id": app, "upload_token": r["result"]["upload_token"],
             "declared_type": doc_type})
    assert r["status"] == "ok", r


def new_application(name: str, amount: int = 5000000) -> str:
    r = rest("dev-officer-ana", "application.create", {"product_code": "SME-WC", "requested_amount_minor": amount,
             "currency": "USD", "requested_term_months": 12, "borrower": "borrower:mekong"})
    app = r["result"]["application_id"]
    biz = rest("dev-officer-ana", "party.add", {"application_id": app, "kind": "business", "role": "applicant",
               "legal_name": name, "registration_number": "0105561234567", "country": "TH"})["result"]["party_id"]
    d = rest("dev-officer-ana", "party.add", {"application_id": app, "kind": "individual", "role": "director",
             "legal_name": "Somchai P.", "date_of_birth": "1980-04-02"})["result"]["party_id"]
    assert rest("dev-officer-ana", "application.submit", {"application_id": app})["status"] == "ok"
    upload(app, biz, "business_registration", mock_doc("business_registration", "registration_number: 0105561234567",
                                                       f"legal_name: {name}"))
    upload(app, biz, "bank_statements", mock_doc("bank_statements", f"account_holder: {name}", "months_covered: 12"))
    upload(app, biz, "proof_of_address", mock_doc("proof_of_address", "address: 88 Riverside Road?"))
    upload(app, biz, "financial_statements", mock_doc("financial_statements", "years_covered: 2"))
    upload(app, d, "identity_document", mock_doc("identity_document", "legal_name: Somchai P.", "date_of_birth: 1980-04-02"))
    for party, scope in [(biz, "kyc"), (biz, "credit_bureau"), (biz, "bank_data"), (d, "kyc")]:
        r = rest("dev-borrower-mekong", "consent.record", {"application_id": app, "party_id": party, "scope": scope,
                 "evidence": {"method": "portal_click"}})
        assert r["status"] == "ok", r
    return app


@unittest.skipUnless(URL, "TAMARIND_URL not set")
class LiveAgentsTest(unittest.TestCase):
    def setUp(self):
        self.state = tempfile.TemporaryDirectory()
        self.addCleanup(self.state.cleanup)

    def run_agents(self):
        for agent, token in [(DocumentAgent(), "dev-agent-documents"), (VerificationAgent(), "dev-agent-verification"),
                             (UnderwritingAgent(), "dev-agent-underwriting")]:
            Worker(agent, TamarindClient(URL, token), Path(self.state.name)).drain()

    def get(self, app):
        return rest("dev-officer-ana", "application.get", {"application_id": app})["result"]["application"]

    def pending(self, app):
        r = rest("dev-officer-chai", "application.get", {"application_id": app})
        self.assertEqual(r["status"], "ok")
        q = rest("dev-officer-chai", "approval.list", {"application_id": app})
        return q["result"]["approval_requests"]

    def test_agents_verify_and_propose(self):
        healthy = new_application("Agentic Trading Co.")
        thin = new_application("Thin Margin THIN Ltd")
        sanctioned = new_application("Blocked SANCTION Imports")
        self.run_agents()

        app = self.get(healthy)
        self.assertEqual(app["state"], "underwriting")
        # The document agent classified all five uploads and checked their fields; the
        # low-confidence address waits for a person.
        self.assertEqual(app["missing_documents"], [])
        self.assertTrue(all(d["status"] == "extracted" for d in app["documents"]))
        review = [e["field"] for e in app["evidence"] if e["status"] == "needs_review"]
        self.assertEqual(review, ["address"])
        self.assertTrue(app["checks_summary"]["all_mandatory_complete"])
        queue = self.pending(healthy)
        self.assertEqual(len(queue), 1)
        proposal = queue[0]
        self.assertEqual(proposal["tool"], "decision.recommend")
        self.assertEqual(proposal["requested_by"], "agent:underwriting")
        self.assertEqual(proposal["proposal"]["outcome"], "approve")
        self.assertTrue(proposal["you_can_decide"])

        # A person confirms; the recommendation is recorded with the agent as proposer.
        r = rest("dev-officer-chai", "approval.respond", {"approval_request_id": proposal["approval_request_id"],
                                                          "decision": "approve"})
        self.assertEqual(r["status"], "ok", r)
        self.assertEqual(self.get(healthy)["recommendation"]["outcome"], "approve")

        self.assertEqual(self.pending(thin)[0]["proposal"]["outcome"], "refer")
        self.assertIn("FREQUENT_OVERDRAFT", self.pending(thin)[0]["proposal"]["reason_codes"])

        # A sanctions hit refers the case to people: the underwriting agent is never woken.
        self.assertEqual(self.get(sanctioned)["state"], "referred")
        self.assertEqual(self.pending(sanctioned), [])

        # Running again does nothing new (cursors and idempotency keys).
        self.run_agents()
        self.assertEqual(len(self.pending(thin)), 1)


if __name__ == "__main__":
    unittest.main()
