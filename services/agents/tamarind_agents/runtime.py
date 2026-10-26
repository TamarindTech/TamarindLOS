"""Event loop: route domain events to agents, one cursor per agent.

Each agent follows the tenant's event feed with its own token and its own
cursor, long-polling so it wakes as soon as an event commits. Handlers are
idempotent (keys derive from the event id), so an event that is processed
twice, after a crash or a restart, replays the original calls instead of
repeating them.

Every event an agent acts on opens an agent run. Its tool calls carry the run id,
so the run can be audited step by step; when the agent hands the case to a person
it creates a task, and the run records which one.
"""

from __future__ import annotations

import json
import logging
import threading
import time
from pathlib import Path
from typing import Any

from . import __version__
from .agents import Agent, Outcome
from .client import GatewayError, TamarindClient

log = logging.getLogger("tamarind.runtime")

MAX_ATTEMPTS = 3
DEFAULT_ASSIGNEE = "role:loan_officer"
RUN_STATUS = {"acted": "succeeded", "skipped": "cancelled", "escalated": "escalated"}


class Cursor:
    """Last processed event sequence number, persisted to a small JSON file."""

    def __init__(self, path: Path) -> None:
        self.path = path
        try:
            self.after = int(json.loads(path.read_text())["after"])
        except (OSError, ValueError, KeyError, TypeError):
            self.after = 0

    def advance(self, seq: int) -> None:
        if seq <= self.after:
            return
        self.after = seq
        self.path.parent.mkdir(parents=True, exist_ok=True)
        tmp = self.path.with_suffix(".tmp")
        tmp.write_text(json.dumps({"after": seq}))
        tmp.replace(self.path)


class Worker:
    """Runs one agent against the feed."""

    def __init__(self, agent: Agent, client: TamarindClient, state_dir: Path, wait: int = 25) -> None:
        self.agent, self.client, self.wait = agent, client, wait
        self.cursor = Cursor(state_dir / f"{agent.name}.json")
        self.outcomes: list[Outcome] = []

    def process(self, event: dict[str, Any]) -> None:
        if not self.agent.wants(event):
            return
        run_id, linked = self.start_run(event)
        self.client.run_id = run_id if linked else None
        try:
            out = self.handle(event)
            task_id = self.escalate(event, out) if out.action == "escalated" else None
        finally:
            self.client.run_id = None
        # If the gateway was unreachable the exception above propagates and the run stays
        # open: the event is retried, agent.run_start replays and the same run continues.
        # A run nobody comes back to is closed as failed after 10 minutes.
        if run_id:
            self.finish_run(event, run_id, RUN_STATUS.get(out.action, "succeeded"), out.detail, task_id, reprocessed=not linked)
        self.outcomes.append(out)
        log.info(json.dumps({"agent": self.agent.name, "event_seq": event["seq"], "application_id": event["aggregate_id"],
                             "run_id": run_id, "action": out.action, "detail": out.detail, "task_id": task_id,
                             "calls": out.calls}))

    def handle(self, event: dict[str, Any]) -> Outcome:
        for attempt in range(1, MAX_ATTEMPTS + 1):
            try:
                return self.agent.handle(event, self.client)
            except GatewayError as e:
                if attempt == MAX_ATTEMPTS:
                    raise
                log.warning("%s: gateway error on event %s (attempt %d): %s", self.agent.name, event["seq"], attempt, e)
                time.sleep(attempt)
            except Exception:  # noqa: BLE001 - a bad event must not stop the agent
                log.exception("%s: handler failed on event %s; skipping it", self.agent.name, event["seq"])
                return Outcome("escalated", "handler error")
        raise AssertionError("unreachable")

    # --- runs and escalation ------------------------------------------------------

    def start_run(self, event: dict[str, Any]) -> tuple[str | None, bool]:
        """Open a run for this event: (run id, whether calls should carry it).

        Work goes ahead without a run if the gateway refuses one. When the event is
        being reprocessed after a crash, the run may already be closed, so calls are
        not linked to it, but it is still closed at the end if it was left open.
        """
        args: dict[str, Any] = {"agent_version": __version__, "trigger_event_id": event["event_id"],
                                "trigger_event_type": event.get("type", "")}
        if event.get("aggregate_type", "application") == "application":
            args["application_id"] = event["aggregate_id"]
        for attempt in (1, 2):
            try:
                r = self.client.call("agent.run_start", args, reason=f"{self.agent.name}: woken by {event.get('type')}",
                                     key=self.agent.key(event, "run"))
            except GatewayError as e:
                log.warning("%s: cannot open a run for event %s: %s", self.agent.name, event["seq"], e)
                return None, False
            if r.ok and r.replayed:
                log.info("%s: event %s reprocessed; run %s not re-attached", self.agent.name, event["seq"], r.result.get("run_id"))
                return r.result.get("run_id"), False
            if r.ok:
                return r.result.get("run_id"), True
            stale = (r.error_details or {}).get("run_id") if r.error_code == "STATE_CONFLICT" else None
            if stale and attempt == 1:
                # A previous run was never closed (the process died first). Close it and go on.
                self.client.call("agent.run_finish", {"run_id": stale, "status": "failed",
                                 "summary": "superseded: the process stopped before closing this run"},
                                 reason=f"{self.agent.name}: closing a run left open", key=self.agent.key(event, "supersede", stale))
                continue
            log.warning("%s: run not recorded for event %s: %s %s", self.agent.name, event["seq"], r.error_code, r.error_message)
            return None, False
        return None, False

    def escalate(self, event: dict[str, Any], out: Outcome) -> str | None:
        """Hand the case to a person as a task (default: the loan officers' queue)."""
        task = dict(out.task or {})
        details = {"agent": self.agent.principal, "event_id": event["event_id"], "detail": out.detail, **(task.get("details") or {})}
        args: dict[str, Any] = {"kind": task.get("kind", "resolve_exception"),
                                "title": (task.get("title") or f"{self.agent.name} agent needs a person: {out.detail}")[:200],
                                "assignee": task.get("assignee", DEFAULT_ASSIGNEE), "details": details}
        if event.get("aggregate_type", "application") == "application":
            args["application_id"] = event["aggregate_id"]
        try:
            r = self.client.call("task.create", args, reason=f"{self.agent.name}: escalating to a person: {out.detail}"[:500],
                                 key=self.agent.key(event, "escalate"))
        except GatewayError as e:
            log.warning("%s: could not create escalation task: %s", self.agent.name, e)
            return None
        if not r.ok:
            log.warning("%s: escalation task refused: %s %s", self.agent.name, r.error_code, r.error_message)
            return None
        return r.result.get("task_id")

    def finish_run(self, event: dict[str, Any], run_id: str, status: str, summary: str, task_id: str | None,
                   reprocessed: bool = False) -> None:
        args: dict[str, Any] = {"run_id": run_id, "status": status, "summary": summary[:1000]}
        if task_id:
            args["escalation_task_id"] = task_id
        for attempt in range(1, MAX_ATTEMPTS + 1):
            try:
                r = self.client.call("agent.run_finish", args, reason=f"{self.agent.name}: run {status}",
                                     key=self.agent.key(event, "run", "finish"))
            except GatewayError as e:
                if attempt == MAX_ATTEMPTS:
                    # The run stays open; the next run on this application supersedes it,
                    # and the gateway closes abandoned runs after a while.
                    log.warning("%s: could not close run %s: %s", self.agent.name, run_id, e)
                    return
                time.sleep(attempt)
                continue
            if not r.ok:
                # After a reprocessed event the run was usually closed the first time round:
                # the close is refused (STATE_CONFLICT) or its key was already used with the
                # first outcome (IDEMPOTENCY_CONFLICT). Neither needs attention.
                expected = reprocessed and r.error_code in ("STATE_CONFLICT", "IDEMPOTENCY_CONFLICT")
                (log.info if expected else log.warning)("%s: could not close run %s: %s %s", self.agent.name, run_id,
                                                        r.error_code, r.error_message)
            return

    def poll(self, wait: int) -> int:
        """Read one page of the feed and process it. Returns the number of events read."""
        page = self.client.events(self.cursor.after, types=list(self.agent.event_types), wait=wait)
        for event in page["events"]:
            self.process(event)
            self.cursor.advance(event["seq"])
        self.cursor.advance(int(page.get("next_after", self.cursor.after)))
        return len(page["events"])

    def drain(self) -> None:
        """Process everything available now, then return (used by tests and `once`)."""
        while self.poll(wait=0):
            pass

    def run(self, stop: threading.Event) -> None:
        backoff = 1
        while not stop.is_set():
            try:
                self.poll(wait=self.wait)
                backoff = 1
            except GatewayError as e:
                log.warning("%s: %s; retrying in %ss", self.agent.name, e, backoff)
                stop.wait(backoff)
                backoff = min(backoff * 2, 30)


def run_forever(workers: list[Worker]) -> None:
    stop = threading.Event()
    threads = [threading.Thread(target=w.run, args=(stop,), name=w.agent.name, daemon=True) for w in workers]
    for t in threads:
        t.start()
    log.info("agents running: %s", ", ".join(w.agent.name for w in workers))
    try:
        while any(t.is_alive() for t in threads):
            time.sleep(1)
    except KeyboardInterrupt:
        stop.set()
        for t in threads:
            t.join(timeout=5)
