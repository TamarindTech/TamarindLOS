"""Credit memo narratives.

The narrative explains a recommendation to the approver. It never decides it:
the outcome and reason codes come from the policy pack's rules, and adverse
action reasons are taken from those reason codes (Reference Architecture §9.4).

`TemplateNarrator` is deterministic and needs nothing. `AnthropicNarrator`
asks a hosted model to draft the memo from the same facts and falls back to the
template on any failure. Only terms, metrics and rule results are sent; no
names, identifiers or documents.
"""

from __future__ import annotations

import json
import logging
import os
import urllib.error
import urllib.request
from typing import Any, Protocol

log = logging.getLogger("tamarind.agents.narrative")

PROMPT_VERSION = "uw-memo@1"


class Narrator(Protocol):
    def write(self, facts: dict[str, Any], outcome: str) -> tuple[str, dict[str, Any] | None]:
        """Return the narrative and, when a model wrote it, the model record for the decision record."""
        ...


def money(minor: Any, currency: str | None) -> str:
    try:
        return f"{currency or ''} {int(minor) / 100:,.2f}".strip()
    except (TypeError, ValueError):
        return "n/a"


class TemplateNarrator:
    def write(self, facts: dict[str, Any], outcome: str) -> tuple[str, dict[str, Any] | None]:
        cur = facts.get("currency")
        m = facts.get("metrics") or {}
        c = facts.get("checks") or {}
        p = facts["policy"]
        lines = [
            f"Policy outcome: {p['outcome'].upper()} ({p['rules_passed']} of {p['rules_total']} rules passed, "
            f"pack {p.get('pack')}@{p.get('version')}).",
            f"Request: {money(facts.get('requested_amount_minor'), cur)} over {facts.get('requested_term_months')} months"
            + (f" for {facts['purpose']}." if facts.get("purpose") else "."),
        ]
        if m:
            lines.append(
                f"Cash flow: DSCR {m.get('dscr')} including the new loan (estimated payment "
                f"{money(m.get('estimated_new_payment_minor'), cur)} a month at the reference rate); annual revenue "
                f"about {money(m.get('annual_revenue_minor'), cur)}; overdrawn on {m.get('negative_balance_days_6m')} day(s) "
                f"in 6 months; {m.get('bank_months')} months of bank data."
            )
        else:
            lines.append("Cash flow: no bank analysis available.")
        lines.append(
            "Checks: "
            + ("all mandatory checks complete" if c.get("all_mandatory_complete") else "mandatory checks incomplete")
            + ("; sanctions match confirmed" if (c.get("sanctions") or {}).get("confirmed_match") else "; no sanctions match")
            + ("; identity verified" if (c.get("kyc") or {}).get("all_passed") else "; identity not fully verified")
            + f"; business verification {(c.get('kyb') or {}).get('status') or 'not run'}"
            + ("; serious bureau default in 24 months." if (c.get("bureau") or {}).get("serious_default_24m")
               else "; no serious bureau default.")
        )
        d = facts.get("documents") or {}
        if d.get("count"):
            lines.append(f"Documents: {d['count']} on file; {d.get('evidence_verified', 0)} extracted fields verified"
                         + (f", {d['evidence_for_review']} waiting for review." if d.get("evidence_for_review") else "."))
        if p.get("failed"):
            lines.append("Failed rules: " + ", ".join(f"{f['rule_id']} {f['reason_code']} ({f['outcome']})" for f in p["failed"]) + ".")
        lines.append({
            "approve": "Recommendation: approve on the requested terms at the product reference rate.",
            "refer": "Recommendation: refer to an approver; the failed rules need human judgement.",
            "decline": "Recommendation: decline; a knock-out rule failed.",
        }.get(outcome, f"Recommendation: {outcome}."))
        lines.append("Drafted by the underwriting agent from policy results; a person must confirm it.")
        return "\n".join(lines), None


class AnthropicNarrator:
    """Drafts the memo with Anthropic's Messages API. Configure with environment variables:

    ANTHROPIC_API_KEY      API key
    TAMARIND_LLM_MODEL     model id to use (required; no default is assumed)
    """

    endpoint = "https://api.anthropic.com/v1/messages"

    def __init__(self, api_key: str, model: str, fallback: Narrator | None = None, timeout: float = 30.0) -> None:
        self.api_key, self.model, self.timeout = api_key, model, timeout
        self.fallback = fallback or TemplateNarrator()

    def write(self, facts: dict[str, Any], outcome: str) -> tuple[str, dict[str, Any] | None]:
        template, _ = self.fallback.write(facts, outcome)
        system = (
            "You are a credit analyst writing a short memo for a loan approver. Use only the facts given. "
            "Do not change the recommended outcome, invent figures, or give legal advice. "
            "Write at most 180 words of plain prose, then one line starting 'Recommendation:'."
        )
        user = ("Facts (JSON):\n" + json.dumps(facts, indent=2, default=str)
                + f"\n\nRecommended outcome (fixed by policy rules): {outcome}\n\nReference memo:\n{template}")
        body = {"model": self.model, "max_tokens": 600, "system": system,
                "messages": [{"role": "user", "content": user}]}
        req = urllib.request.Request(self.endpoint, data=json.dumps(body).encode(), method="POST")
        req.add_header("x-api-key", self.api_key)
        req.add_header("anthropic-version", "2023-06-01")
        req.add_header("content-type", "application/json")
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as res:
                data = json.loads(res.read())
            text = "".join(b.get("text", "") for b in data.get("content", []) if b.get("type") == "text").strip()
            if not text or len(text) > 7000:
                raise ValueError("empty or oversized narrative")
        except (urllib.error.URLError, TimeoutError, ValueError, KeyError) as e:
            log.warning("LLM narrative failed (%s); using the template", e)
            return template, None
        text += "\n\n(Narrative drafted by a language model; outcome and reason codes come from policy rules.)"
        return text, {"kind": "llm", "id": f"anthropic/{self.model}", "prompt_version": PROMPT_VERSION}


def narrator_from_env() -> Narrator:
    provider = os.environ.get("TAMARIND_LLM_PROVIDER", "").lower()
    if provider == "anthropic":
        key, model = os.environ.get("ANTHROPIC_API_KEY"), os.environ.get("TAMARIND_LLM_MODEL")
        if key and model:
            return AnthropicNarrator(key, model)
        log.warning("TAMARIND_LLM_PROVIDER=anthropic needs ANTHROPIC_API_KEY and TAMARIND_LLM_MODEL; using the template")
    elif provider:
        log.warning("unknown TAMARIND_LLM_PROVIDER %r; using the template", provider)
    return TemplateNarrator()
