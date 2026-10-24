"""Event loop: route domain events to agents, one cursor per agent.

Each agent follows the tenant's event feed with its own token and its own
cursor, long-polling so it wakes as soon as an event commits. Handlers are
idempotent (keys derive from the event id), so an event that is processed
twice, after a crash or a restart, replays the original calls instead of
repeating them.
"""

from __future__ import annotations

import json
import logging
import threading
import time
from pathlib import Path
from typing import Any

from .agents import Agent, Outcome
from .client import GatewayError, TamarindClient

log = logging.getLogger("tamarind.runtime")

MAX_ATTEMPTS = 3


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
        for attempt in range(1, MAX_ATTEMPTS + 1):
            try:
                out = self.agent.handle(event, self.client)
            except GatewayError as e:
                if attempt == MAX_ATTEMPTS:
                    raise
                log.warning("%s: gateway error on event %s (attempt %d): %s", self.agent.name, event["seq"], attempt, e)
                time.sleep(attempt)
                continue
            except Exception:  # noqa: BLE001 - a bad event must not stop the agent
                log.exception("%s: handler failed on event %s; skipping it", self.agent.name, event["seq"])
                out = Outcome("escalated", "handler error")
            self.outcomes.append(out)
            log.info(json.dumps({"agent": self.agent.name, "event_seq": event["seq"], "application_id": event["aggregate_id"],
                                 "action": out.action, "detail": out.detail, "calls": out.calls}))
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
