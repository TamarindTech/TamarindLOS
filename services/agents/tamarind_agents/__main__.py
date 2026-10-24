"""Command line: python -m tamarind_agents [run|once] [--agents verification,underwriting]

Configuration (environment):
  TAMARIND_URL                  gateway base URL (default http://localhost:8080)
  TAMARIND_VERIFICATION_TOKEN   bearer token for agent:verification (default: development token)
  TAMARIND_UNDERWRITING_TOKEN   bearer token for agent:underwriting (default: development token)
  TAMARIND_AGENTS_STATE         directory for event cursors (default .tamarind-agents)
  TAMARIND_LLM_PROVIDER         optional: "anthropic" to draft memos with a model (see narrative.py)
"""

from __future__ import annotations

import argparse
import logging
import os
import sys
from pathlib import Path

from .agents import builtin_agents
from .client import GatewayError, TamarindClient
from .narrative import narrator_from_env
from .runtime import Worker, run_forever

DEV_TOKENS = {"verification": "dev-agent-verification", "underwriting": "dev-agent-underwriting"}


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(prog="tamarind_agents", description="Run Tamarind's built-in agents.")
    ap.add_argument("command", nargs="?", default="run", choices=["run", "once"],
                    help="run: follow the event feed; once: process what is there now and exit")
    ap.add_argument("--agents", default="verification,underwriting", help="comma-separated agent names")
    ap.add_argument("--url", default=os.environ.get("TAMARIND_URL", "http://localhost:8080"))
    ap.add_argument("--state", default=os.environ.get("TAMARIND_AGENTS_STATE", ".tamarind-agents"))
    args = ap.parse_args(argv)

    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")
    available = builtin_agents(narrator_from_env())
    workers = []
    for name in [n.strip() for n in args.agents.split(",") if n.strip()]:
        if name not in available:
            ap.error(f"unknown agent {name!r}; choose from {', '.join(available)}")
        token = os.environ.get(f"TAMARIND_{name.upper()}_TOKEN", DEV_TOKENS[name])
        client = TamarindClient(args.url, token)
        try:
            tools = client.list_tools()
        except GatewayError as e:
            if args.command == "once":
                print(f"cannot reach the gateway: {e}", file=sys.stderr)
                return 1
            logging.warning("%s: gateway not ready (%s); will keep retrying", name, e)
            tools = []
        if tools:
            logging.info("%s: %d tools available: %s", name, len(tools), ", ".join(tools))
        workers.append(Worker(available[name], client, Path(args.state)))

    if args.command == "once":
        for w in workers:
            w.drain()
        return 0
    run_forever(workers)
    return 0


if __name__ == "__main__":
    sys.exit(main())
